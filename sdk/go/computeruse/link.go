package computeruse

// The generated bindings (computeruse.go, computeruse.h) call into
// libcomputeruse, the Rust SDK built as a C library. This file tells cgo
// what to link; where the library is comes from CGO_LDFLAGS:
//
//	CGO_LDFLAGS="-L/path/to/dir/with/libcomputeruse.a" go build ./...
//
// The system libraries listed are what the static library needs. They are
// harmless when the shared library is linked instead.

/*
#cgo LDFLAGS: -lcomputeruse
#cgo linux LDFLAGS: -lm -ldl -lpthread
#cgo darwin LDFLAGS: -framework Security -framework CoreFoundation -framework SystemConfiguration
*/
import "C"
