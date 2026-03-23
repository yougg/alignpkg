# Align and sort golang imported packages by group.

## Install

```shell
go install github.com/yougg/alignpkg@latest
```

## Features

- Automatically split your imports in three categories: inbuilt, external and local.
- Written fully in Golang, no dependencies in runtime, works on any platform.
- Detects Go module name automatically.
- Orders your imports alphabetically.
- Removes additional line breaks.
- No more manually fixing import orders.
- Load the standard go module only once for all tasks. (complete more quickly).
- Support secondary package prefix (2-part-package) which will sort import into 4 groups.
- Cache standard package info to reduce parse time cost and run more quickly.
- Auto-detect local module path from file location (traverse up directory tree to find go.mod).

## Usage

```
usage: alignpkg [flags] [path ...]
  -l    write results to stdout (default false)
  -local string
        put imports beginning with this string after 3rd-party packages; comma-separated list 
(default tries to get module name of current directory)
  -v    verbose logging (default false)
  -w    write result to (source) file (default false)
```

Import packages will be sorted according to their categories.

```
alignpkg -v -w ./..
```

For example:

```go
package main

import (
	"fmt"
	"log"
	APZ "bitbucket.org/example/package/name"
	APA "bitbucket.org/example/package/name"
	"github.com/yougg/alignpkg/package2"
	"github.com/yougg/alignpkg/package1"
)
import (
	"net/http/httptest"
)

import "bitbucket.org/example/package/name2"
import "bitbucket.org/example/package/name3"
import "bitbucket.org/example/package/name4"
```

it will be transformed into:

```go
package main

import (
    "fmt"
    "log"
    "net/http/httptest"

    APA "bitbucket.org/example/package/name"
    APZ "bitbucket.org/example/package/name"
    "bitbucket.org/example/package/name2"
    "bitbucket.org/example/package/name3"
    "bitbucket.org/example/package/name4"

    "github.com/yougg/alignpkg/package1"
    "github.com/yougg/alignpkg/package2"
)
```
