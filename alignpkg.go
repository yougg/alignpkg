package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/dstutil"
	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

type (
	// impModel is used for storing import information
	impModel struct {
		path           string
		localReference string
		decs           *dst.ImportSpecDecorations
	}

	impManager struct {
		groups  []*impGroup
		decs    []string
		isBlock bool
	}

	impGroup struct {
		models []*impModel
	}
)

const (
	GroupStandard int = iota // 0
	GroupThird
	GroupSecond
	GroupLocal
	GroupCount

	unixNewLine    = "\n"
	windowsNewLine = "\r\n"
)

var (
	unixLineBreak    = []byte{'\n'}
	windowsLineBreak = []byte{'\r', '\n'}
	wrongLineBreak   = []byte{'\r', '\r', '\n'}
)

var (
	list            bool
	write           bool
	updateCache     bool
	verbose         bool
	localPrefix     string
	secondPrefix    string
	transformSingle string

	standardPackages = make(map[string]struct{})
	cacheManager     *CacheManager
)

func init() {
	flag.BoolVar(&list, "l", false, "write results to stdout")
	flag.BoolVar(&list, "list", false, "write results to stdout")
	flag.BoolVar(&write, "w", false, "write result to (source) file instead of stdout")
	flag.BoolVar(&write, "write", false, "write result to (source) file instead of stdout")
	flag.BoolVar(&updateCache, "u", false, "update the standard package cache for current Go version")
	flag.BoolVar(&verbose, "v", false, "verbose logging")
	flag.StringVar(&localPrefix, "local", ``, "put imports beginning with this string after 3rd-party packages; comma-separated list")
	flag.StringVar(&secondPrefix, "second", ``, "put imports beginning with this string after 3rd-party packages; comma-separated list")
	flag.StringVar(&transformSingle, "single", "keep", "transform single import format: keep (default), oneline, group")
}

func (g *impGroup) append(model *impModel) {
	g.models = append(g.models, model)
}

func newImpManager() *impManager {
	groups := make([]*impGroup, GroupCount)
	for idx := range groups {
		groups[idx] = &impGroup{
			models: []*impModel{},
		}
	}
	return &impManager{groups: groups}
}

func (m *impManager) Standard() *impGroup {
	return m.groups[GroupStandard]
}

func (m *impManager) Local() *impGroup {
	return m.groups[GroupLocal]
}

func (m *impManager) ThirdPart() *impGroup {
	return m.groups[GroupThird]
}

func (m *impManager) SecondPart() *impGroup {
	return m.groups[GroupSecond]
}

// string is used to get a string representation of an import
func (m impModel) string() string {
	s := m.path
	if m.localReference != `` {
		s = m.localReference + ` ` + m.path
	}

	if m.decs != nil {
		// Add Start decorations (comments before)
		if len(m.decs.Start) > 0 {
			var starts []string
			for _, start := range m.decs.Start {
				trimmed := strings.TrimSpace(start)
				if trimmed != `` {
					starts = append(starts, trimmed)
				}
			}
			if len(starts) > 0 {
				startStr := strings.Join(starts, "\n\t")
				s = startStr + "\n\t" + s
			}
		}
		// Add End decorations (trailing comments)
		if len(m.decs.End) > 0 {
			s += ` ` + strings.Join(m.decs.End, ` `)
		}
	}

	return s
}

// detectLineEnding detects the line ending used in the source.
// Returns "\r\n" if CRLF is found, otherwise returns "\n".
func detectLineEnding(src []byte) string {
	if bytes.Contains(src, windowsLineBreak) {
		return windowsNewLine
	}
	return unixNewLine
}

// main is the entry point of the program
func main() {
	err := goImportsSortMain()
	if err != nil {
		slog.Error("failed", `err`, err)
		os.Exit(1)
	}
}

// goImportsSortMain checks passed flags and starts processing files
func goImportsSortMain() error {
	flag.Usage = func() {
		_, _ = fmt.Fprintf(os.Stderr, "usage: alignpkg [flags] [path ...]\n")
		flag.PrintDefaults()
		os.Exit(2)
	}
	paths := parseFlags()

	// Validate transformSingle flag
	switch transformSingle {
	case "keep", "oneline", "group":
		// valid
	default:
		return fmt.Errorf("invalid -single value: %s (must be keep, oneline, or group)", transformSingle)
	}

	if verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	} else {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// Initialize cache manager
	var err error
	cacheManager, err = newCacheManager()
	if err != nil {
		slog.Warn("failed to initialize cache manager", `err`, err)
	}

	// Handle cache update flag
	if updateCache {
		if cacheManager == nil {
			return errors.New("cache manager not available")
		}
		if err = cacheManager.update(); err != nil {
			return fmt.Errorf("failed to update cache: %w", err)
		}
		slog.Info("Cache updated", `version`, cacheManager.version)
		return nil
	}

	if localPrefix == `` {
		slog.Info("no prefix found, using module name")

		moduleName := getModuleName()
		if moduleName != `` {
			localPrefix = moduleName
		} else {
			slog.Info("module name not found. skipping localprefix")
		}
	}

	if len(paths) == 0 {
		return errors.New("please enter a path to fix")
	}

	// load it in global
	err = loadStandardPackages()
	if err != nil {
		panic(err)
	}

	for _, path := range paths {
		switch dir, statErr := os.Stat(path); {
		case statErr != nil:
			return statErr
		case dir.IsDir():
			return walkDir(path)
		default:
			_, err = processFile(path, nil, os.Stdout)
			return err
		}
	}

	return nil
}

// parseFlags parses command line flags and returns the paths to process.
// It's a var so that custom implementations can replace it in other files.
var parseFlags = func() []string {
	flag.Parse()

	return flag.Args()
}

// isGoFile checks if the file is a go file & not a directory
func isGoFile(f os.FileInfo) bool {
	name := f.Name()
	return !f.IsDir() && !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".go")
}

// walkDir walks through a path, processing all go files recursively in a directory
func walkDir(path string) error {
	return filepath.Walk(
		path,
		func(path string, f os.FileInfo, err error) error {
			if err == nil && isGoFile(f) {
				_, err = processFile(path, nil, os.Stdout)
			}
			return err
		},
	)
}

// processFile reads a file and processes the content, then checks if they're equal.
func processFile(filename string, in io.Reader, out io.Writer) ([]byte, error) {
	slog.Debug("processing imported packages", `file`, filename)

	if in == nil {
		f, err := os.Open(filename)
		if err != nil {
			return nil, err
		}
		defer closeFile(f)
		in = f
	}

	src, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}

	res, err := process(src, filename)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(src, res) {
		// formatting has changed
		if list {
			_, _ = fmt.Fprintln(out, string(res))
		}
		if write {
			err = os.WriteFile(filename, res, 0)
			if err != nil {
				return nil, err
			}
		}
		if !list && !write {
			return res, nil
		}
	}

	return res, err
}

// closeFile tries to close a File and prints an error when it can't
func closeFile(file *os.File) {
	err := file.Close()
	if err != nil {
		slog.Error("could not close file")
	}
}

// process processes the source of a file, categorizing the imports
// filePath is used to detect the local module path for the file
func process(src []byte, filePath string) (output []byte, err error) {
	var (
		fileSet          = token.NewFileSet()
		convertedImports *impManager
		node             *dst.File
	)

	node, err = decorator.ParseFile(fileSet, ``, src, parser.ParseComments)
	if err != nil {
		panic(err)
	}

	// Detect original line ending
	eol := detectLineEnding(src)

	// Determine local prefix for this file
	fileLocalPrefix := localPrefix
	if fileLocalPrefix == `` && filePath != `` {
		// Auto-detect module path from file location
		fileLocalPrefix = findModulePath(filePath)
	}

	convertedImports, err = convertImportsToSlice(node, fileLocalPrefix)
	if err != nil {
		panic(err)
	}
	if convertedImports.countImports() == 0 {
		return src, err
	}

	convertedImports.alignPkg()
	convertedToGo := convertedImports.convertImportsToGo(eol)
	output, err = replaceImports(convertedToGo, node, eol)
	if err != nil {
		panic(err)
	}

	// Ensure the entire output uses the original line ending if it was CRLF
	if eol == windowsNewLine {
		output = bytes.ReplaceAll(output, unixLineBreak, windowsLineBreak)
		// bytes.ReplaceAll might have created \r\r\n if some lines already had \r\n
		output = bytes.ReplaceAll(output, wrongLineBreak, windowsLineBreak)
	}

	return output, err
}

// replaceImports replaces existing imports and handles multiple import statements
func replaceImports(newImports []byte, node *dst.File, eol string) ([]byte, error) {
	var (
		output []byte
		err    error
		buf    bytes.Buffer
	)

	// remove + update
	dstutil.Apply(node, func(cr *dstutil.Cursor) bool {
		n := cr.Node()

		if decl, ok := n.(*dst.GenDecl); ok && decl.Tok == token.IMPORT {
			cr.Delete()
		}

		return true
	}, nil)

	err = decorator.Fprint(&buf, node)

	if err == nil {
		packageName := node.Name.Name
		output = bytes.Replace(buf.Bytes(), []byte("package "+packageName), append([]byte("package "+packageName+eol+eol), newImports...), 1)
	} else {
		slog.Error("replace error", `err`, err)
	}

	return output, err
}

func (m *impManager) alignPkg() {
	for _, g := range m.groups {
		g.alignPkg()
	}
}

// alignPkg sorts multiple imports by import name & prefix
func (g *impGroup) alignPkg() {
	var imports = g.models
	for x := 0; x < len(imports); x++ {
		sort.Slice(imports, func(i, j int) bool {
			if imports[i].path != imports[j].path {
				return imports[i].path < imports[j].path
			}
			return imports[i].localReference < imports[j].localReference
		})
	}
}

// convertImportsToGo generates output for correct categorized import statements
func (m *impManager) convertImportsToGo(eol string) []byte {
	prefix := ``
	if len(m.decs) > 0 {
		// Trim trailing empty strings from decs to avoid double blank lines
		last := len(m.decs) - 1
		for last >= 0 && strings.TrimSpace(m.decs[last]) == `` {
			last--
		}
		if last >= 0 {
			prefix = strings.Join(m.decs[:last+1], eol) + eol
		}
	}

	useOneLine := false
	if m.countImports() == 1 {
		switch transformSingle {
		case "oneline":
			useOneLine = true
		case "group":
			useOneLine = false
		case "keep":
			useOneLine = !m.isBlock
		}
	}

	if useOneLine {
		for _, group := range m.groups {
			if group.countImports() == 1 {
				imp := group.models[0]
				s := imp.path
				if imp.localReference != `` {
					s = imp.localReference + ` ` + imp.path
				}

				if imp.decs != nil {
					// Add Start decorations (comments before)
					if len(imp.decs.Start) > 0 {
						start := strings.Join(imp.decs.Start, eol)
						s = start + eol + "import " + s
					} else {
						s = "import " + s
					}
					// Add End decorations (trailing comments)
					if len(imp.decs.End) > 0 {
						s += ` ` + strings.Join(imp.decs.End, ` `)
					}
				} else {
					s = "import " + s
				}
				return []byte(prefix + s)
			}
		}
	}

	output := prefix + "import ("

	for _, group := range m.groups {
		if group.countImports() == 0 {
			continue
		}
		output += eol
		for _, imp := range group.models {
			output += fmt.Sprintf("\t%v"+eol, imp.string())
		}
	}

	output += ")"

	return []byte(output)
}

func (g *impGroup) countImports() int {
	return len(g.models)
}

// countImports count the total number of imports of a [][]impModel
func (m *impManager) countImports() int {
	count := 0
	for _, group := range m.groups {
		count += group.countImports()
	}
	return count
}

// convertImportsToSlice parses the file with AST and gets all imports
// localPrefix is the module prefix to identify local packages
func convertImportsToSlice(node *dst.File, localPrefix string) (*impManager, error) {
	importCategories := newImpManager()

	// Capture declaration-level decorations from all GenDecl import nodes
	for _, decl := range node.Decls {
		if gen, ok := decl.(*dst.GenDecl); ok && gen.Tok == token.IMPORT {
			if len(gen.Decs.Start) > 0 {
				importCategories.decs = append(importCategories.decs, gen.Decs.Start...)
			}
			if gen.Lparen {
				importCategories.isBlock = true
			}
		}
	}

	for _, importSpec := range node.Imports {
		impName := importSpec.Path.Value
		impNameWithoutQuotes := strings.Trim(impName, `"`)
		locName := importSpec.Name

		var locImpModel impModel
		if locName != nil {
			locImpModel.localReference = locName.Name
		}
		locImpModel.path = impName
		locImpModel.decs = &importSpec.Decs

		if localPrefix != `` && isLocalPackageWithPrefix(impName, localPrefix) {
			var group = importCategories.Local()
			group.append(&locImpModel)
		} else if isStandardPackage(impNameWithoutQuotes) {
			var group = importCategories.Standard()
			group.append(&locImpModel)
		} else if isSecondPackage(impNameWithoutQuotes) {
			var group = importCategories.SecondPart()
			group.append(&locImpModel)
		} else {
			var group = importCategories.ThirdPart()
			group.append(&locImpModel)
		}
	}

	return importCategories, nil
}

func isSecondPackage(impName string) bool {
	if secondPrefix != `` {
		// name with " or not
		if strings.HasPrefix(impName, secondPrefix) || strings.HasPrefix(impName, `"`+secondPrefix) {
			return true
		}
	}
	return false
}

func isLocalPackage(impName string) bool {
	// name with " or not
	if strings.HasPrefix(impName, localPrefix) || strings.HasPrefix(impName, `"`+localPrefix) {
		return true
	}
	return false
}

// isLocalPackageWithPrefix checks if the import is a local package using the given prefix
func isLocalPackageWithPrefix(impName string, prefix string) bool {
	if prefix == `` {
		return false
	}
	// name with " or not
	if strings.HasPrefix(impName, prefix) || strings.HasPrefix(impName, `"`+prefix) {
		return true
	}
	return false
}

type PackageInfo struct {
	Data    map[string]struct{} `json:"data"`
	Version string              `json:"version"`
}

// CacheManager handles version-aware cache operations
type CacheManager struct {
	cacheDir string
	version  string
}

// newCacheManager creates a new CacheManager for the current Go version
func newCacheManager() (*CacheManager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cacheDir := filepath.Join(homeDir, ".cache", "alignpkg")
	version := runtime.Version()

	return &CacheManager{
		cacheDir: cacheDir,
		version:  version,
	}, nil
}

// getCacheFile returns the version-specific cache file path
func (c *CacheManager) getCacheFile() string {
	// Sanitize version for filename (replace spaces and special chars)
	safeVersion := strings.ReplaceAll(c.version, ` `, `_`)
	return filepath.Join(c.cacheDir, safeVersion+".json")
}

// getOldCachePath returns the old single-file cache path for migration
func (c *CacheManager) getOldCachePath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".cache", "alignpkg.json")
}

// read loads the cache for the current Go version
func (c *CacheManager) read() (*PackageInfo, error) {
	cacheFile := c.getCacheFile()

	if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
		return nil, err
	}

	bs, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, err
	}

	var info PackageInfo
	if err = json.Unmarshal(bs, &info); err != nil {
		return nil, err
	}

	slog.Info("load standard package cache", `file`, cacheFile)
	return &info, nil
}

// write saves the cache for the current Go version
func (c *CacheManager) write(pkgs map[string]struct{}) error {
	// Ensure the cache directory exists
	if err := os.MkdirAll(c.cacheDir, 0755); err != nil {
		return err
	}

	cacheFile := c.getCacheFile()
	info := PackageInfo{
		Data:    make(map[string]struct{}),
		Version: c.version,
	}
	for k, v := range pkgs {
		info.Data[k] = v
	}

	bs, err := json.Marshal(info)
	if err != nil {
		return err
	}

	if err = os.WriteFile(cacheFile, bs, 0644); err != nil {
		return err
	}

	slog.Info("write standard package cache", `file`, cacheFile)
	return nil
}

// update forces a cache refresh for the current Go version
func (c *CacheManager) update() error {
	pkgs, err := packages.Load(nil, "std")
	if err != nil {
		return err
	}

	loadedPkgs := make(map[string]struct{})
	for _, p := range pkgs {
		loadedPkgs[p.PkgPath] = struct{}{}
	}

	return c.write(loadedPkgs)
}

// loadOrFetch loads from cache if available, otherwise fetches and caches
func (c *CacheManager) loadOrFetch() (map[string]struct{}, error) {
	// Try to read from the cache first
	info, err := c.read()
	if err == nil && info != nil {
		return info.Data, nil
	}

	// Cache miss or error - fetch fresh data
	pkgs, err := packages.Load(nil, "std")
	if err != nil {
		return nil, err
	}

	loadedPkgs := make(map[string]struct{})
	for _, p := range pkgs {
		loadedPkgs[p.PkgPath] = struct{}{}
	}

	// Write to cache
	if err = c.write(loadedPkgs); err != nil {
		slog.Warn("failed to write cache", `err`, err)
	}

	return loadedPkgs, nil
}

// loadStandardPackages tries to fetch all golang std packages
func loadStandardPackages() error {
	// Initialize cacheManager if not already done
	if cacheManager == nil {
		var err error
		cacheManager, err = newCacheManager()
		if err != nil {
			slog.Warn("failed to initialize cache manager", `err`, err)
		}
	}

	// Use CacheManager if available
	if cacheManager != nil {
		pkgs, err := cacheManager.loadOrFetch()
		if err != nil {
			return err
		}
		for k, v := range pkgs {
			standardPackages[k] = v
		}
		return nil
	}

	// Fallback: load directly without cache
	pkgs, err := packages.Load(nil, "std")
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		standardPackages[p.PkgPath] = struct{}{}
	}
	return nil
}

// isStandardPackage checks if a package string is included in the standardPackages map
func isStandardPackage(pkg string) bool {
	_, ok := standardPackages[pkg]
	return ok
}

// getModuleName parses the GOMOD name
func getModuleName() string {
	root, err := os.Getwd()
	if err != nil {
		slog.Error("error when getting root path", `err`, err)
		return ``
	}

	goModBytes, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		slog.Error("error when reading mod file", `err`, err)
		return ``
	}

	modName := modfile.ModulePath(goModBytes)

	return modName
}

// findModulePath searches for go.mod starting from the given path,
// traversing up the directory tree until found or reaching the root.
// Returns the module path from go.mod, or empty string if not found.
func findModulePath(startPath string) string {
	// Get the absolute path
	absPath, err := filepath.Abs(startPath)
	if err != nil {
		slog.Error("error when getting absolute path", `err`, err)
		return ``
	}

	// If it's a file, start from its directory
	info, err := os.Stat(absPath)
	if err == nil && !info.IsDir() {
		absPath = filepath.Dir(absPath)
	}

	// Traverse up the directory tree
	currentPath := absPath
	for {
		goModPath := filepath.Join(currentPath, "go.mod")
		if _, err = os.Stat(goModPath); err == nil {
			// Found go.mod, parse it
			goModBytes, err := os.ReadFile(goModPath)
			if err != nil {
				slog.Error("error when reading mod file", `err`, err)
				return ``
			}
			modName := modfile.ModulePath(goModBytes)
			slog.Debug("found module", `name`, modName, `path`, goModPath)
			return modName
		}

		// Move up one directory
		parentPath := filepath.Dir(currentPath)
		if parentPath == currentPath {
			// Reached root, no go.mod found
			slog.Debug("no go.mod found in directory tree")
			return ``
		}
		currentPath = parentPath
	}
}
