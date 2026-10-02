# Computer Use SDK for Go

Go client for [Computer Use](https://computeruse.site): serverless,
resumable desktop containers that an agent drives with one tool, `run_js`.

```go
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/r33drichards/computer-use/sdk/go/computeruse"
)

func main() {
	client, err := computeruse.ClientWithToken(os.Getenv("COMPUTERUSE_API_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}

	// Create a desktop, run JavaScript in it, put it to sleep.
	name := "demo"
	session, err := client.CreateSession(computeruse.CreateSessionRequest{Name: &name})
	if err != nil {
		log.Fatal(err)
	}
	result, err := session.RunJs("console.log(6 * 7)")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(result.Output)
	if _, err := session.Sleep(); err != nil {
		log.Fatal(err)
	}
}
```

The token is an API token (`bjs_...`) from the **API tokens** page of the
app. Calls block; run them in goroutines for concurrency. Errors work with
`errors.Is` and `errors.As`:

```go
_, err := client.GetSession("s-abcde")
if errors.Is(err, computeruse.ErrComputerUseErrorNotFound) { ... }

var conflict *computeruse.ComputerUseErrorConflict
if errors.As(err, &conflict) { fmt.Println(conflict.Message) }
```

## It needs cgo and a library

This is the Rust SDK behind [UniFFI](https://mozilla.github.io/uniffi-rs/)
bindings (`uniffi-bindgen-go`). The Go code calls a C library,
`libcomputeruse`, so:

- cgo must be on (`CGO_ENABLED=1`, the default when a C compiler is there),
  which rules out `CGO_ENABLED=0` builds and makes cross-compiling need a C
  cross-compiler;
- the linker has to be told where the library is.

Each release has a static library per platform. `install-lib.sh` fetches the
one for this machine and prints the setting:

```bash
go get github.com/r33drichards/computer-use/sdk/go@v0.1.0
curl -fsSL https://raw.githubusercontent.com/r33drichards/computer-use/main/sdk/go/install-lib.sh | sh -s -- 0.1.0 ./lib
export CGO_LDFLAGS="-L$PWD/lib"
go build ./...
```

Linked statically, the program has no library to ship beside it. Platforms:
Linux amd64 and arm64 (glibc 2.35 or newer), macOS arm64 and amd64.

To build the library from source instead: `cargo build --release -p
computeruse-sdk` in `sdk/`, then `CGO_LDFLAGS="-L<repo>/sdk/target/release"`.

`computeruse/computeruse.go` and `computeruse.h` are generated
(`sdk/scripts/bindings.sh go`); `link.go` and the test are not.

Reference: <https://computeruse.site/reference/sdk>. Licence: Apache-2.0.
