package computeruse

// #include <computeruse.h>
import "C"

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime"
	"runtime/cgo"
	"sync/atomic"
	"unsafe"
)

// This is needed, because as of go 1.24
// type RustBuffer C.RustBuffer cannot have methods,
// RustBuffer is treated as non-local type
type GoRustBuffer struct {
	inner C.RustBuffer
}

type RustBufferI interface {
	AsReader() *bytes.Reader
	Free()
	ToGoBytes() []byte
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

// C.RustBuffer fields exposed as an interface so they can be accessed in different Go packages.
// See https://github.com/golang/go/issues/13467
type ExternalCRustBuffer interface {
	Data() unsafe.Pointer
	Len() uint64
	Capacity() uint64
}

func RustBufferFromC(b C.RustBuffer) ExternalCRustBuffer {
	return GoRustBuffer{
		inner: b,
	}
}

func CFromRustBuffer(b ExternalCRustBuffer) C.RustBuffer {
	return C.RustBuffer{
		capacity: C.uint64_t(b.Capacity()),
		len:      C.uint64_t(b.Len()),
		data:     (*C.uchar)(b.Data()),
	}
}

func RustBufferFromExternal(b ExternalCRustBuffer) GoRustBuffer {
	return GoRustBuffer{
		inner: C.RustBuffer{
			capacity: C.uint64_t(b.Capacity()),
			len:      C.uint64_t(b.Len()),
			data:     (*C.uchar)(b.Data()),
		},
	}
}

func (cb GoRustBuffer) Capacity() uint64 {
	return uint64(cb.inner.capacity)
}

func (cb GoRustBuffer) Len() uint64 {
	return uint64(cb.inner.len)
}

func (cb GoRustBuffer) Data() unsafe.Pointer {
	return unsafe.Pointer(cb.inner.data)
}

func (cb GoRustBuffer) AsReader() *bytes.Reader {
	b := unsafe.Slice((*byte)(cb.inner.data), C.uint64_t(cb.inner.len))
	return bytes.NewReader(b)
}

func (cb GoRustBuffer) Free() {
	rustCall(func(status *C.RustCallStatus) bool {
		C.ffi_computeruse_rustbuffer_free(cb.inner, status)
		return false
	})
}

func (cb GoRustBuffer) ToGoBytes() []byte {
	return C.GoBytes(unsafe.Pointer(cb.inner.data), C.int(cb.inner.len))
}

func stringToRustBuffer(str string) C.RustBuffer {
	return bytesToRustBuffer([]byte(str))
}

func bytesToRustBuffer(b []byte) C.RustBuffer {
	if len(b) == 0 {
		return C.RustBuffer{}
	}
	// We can pass the pointer along here, as it is pinned
	// for the duration of this call
	foreign := C.ForeignBytes{
		len:  C.int(len(b)),
		data: (*C.uchar)(unsafe.Pointer(&b[0])),
	}

	return rustCall(func(status *C.RustCallStatus) C.RustBuffer {
		return C.ffi_computeruse_rustbuffer_from_bytes(foreign, status)
	})
}

type BufLifter[GoType any] interface {
	Lift(value RustBufferI) GoType
}

type BufLowerer[GoType any] interface {
	Lower(value GoType) C.RustBuffer
}

type BufReader[GoType any] interface {
	Read(reader io.Reader) GoType
}

type BufWriter[GoType any] interface {
	Write(writer io.Writer, value GoType)
}

func LowerIntoRustBuffer[GoType any](bufWriter BufWriter[GoType], value GoType) C.RustBuffer {
	// This might be not the most efficient way but it does not require knowing allocation size
	// beforehand
	var buffer bytes.Buffer
	bufWriter.Write(&buffer, value)

	bytes, err := io.ReadAll(&buffer)
	if err != nil {
		panic(fmt.Errorf("reading written data: %w", err))
	}
	return bytesToRustBuffer(bytes)
}

func LiftFromRustBuffer[GoType any](bufReader BufReader[GoType], rbuf RustBufferI) GoType {
	defer rbuf.Free()
	reader := rbuf.AsReader()
	item := bufReader.Read(reader)
	if reader.Len() > 0 {
		// TODO: Remove this
		leftover, _ := io.ReadAll(reader)
		panic(fmt.Errorf("Junk remaining in buffer after lifting: %s", string(leftover)))
	}
	return item
}

func rustCallWithError[E any, U any](converter BufReader[E], callback func(*C.RustCallStatus) U) (U, E) {
	var status C.RustCallStatus
	returnValue := callback(&status)
	err := checkCallStatus(converter, status)
	return returnValue, err
}

func checkCallStatus[E any](converter BufReader[E], status C.RustCallStatus) E {
	switch status.code {
	case 0:
		var zero E
		return zero
	case 1:
		return LiftFromRustBuffer(converter, GoRustBuffer{inner: status.errorBuf})
	case 2:
		// when the rust code sees a panic, it tries to construct a rustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{inner: status.errorBuf})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		panic(fmt.Errorf("unknown status code: %d", status.code))
	}
}

func checkCallStatusUnknown(status C.RustCallStatus) error {
	switch status.code {
	case 0:
		return nil
	case 1:
		panic(fmt.Errorf("function not returning an error returned an error"))
	case 2:
		// when the rust code sees a panic, it tries to construct a C.RustBuffer
		// with the message.  but if that code panics, then it just sends back
		// an empty buffer.
		if status.errorBuf.len > 0 {
			panic(fmt.Errorf("%s", FfiConverterStringINSTANCE.Lift(GoRustBuffer{
				inner: status.errorBuf,
			})))
		} else {
			panic(fmt.Errorf("Rust panicked while handling Rust panic"))
		}
	default:
		return fmt.Errorf("unknown status code: %d", status.code)
	}
}

func rustCall[U any](callback func(*C.RustCallStatus) U) U {
	returnValue, err := rustCallWithError[error](nil, callback)
	if err != nil {
		panic(err)
	}
	return returnValue
}

type NativeError interface {
	AsError() error
}

func writeInt8(writer io.Writer, value int8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint8(writer io.Writer, value uint8) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt16(writer io.Writer, value int16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint16(writer io.Writer, value uint16) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt32(writer io.Writer, value int32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint32(writer io.Writer, value uint32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeInt64(writer io.Writer, value int64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeUint64(writer io.Writer, value uint64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat32(writer io.Writer, value float32) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func writeFloat64(writer io.Writer, value float64) {
	if err := binary.Write(writer, binary.BigEndian, value); err != nil {
		panic(err)
	}
}

func readInt8(reader io.Reader) int8 {
	var result int8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint8(reader io.Reader) uint8 {
	var result uint8
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt16(reader io.Reader) int16 {
	var result int16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint16(reader io.Reader) uint16 {
	var result uint16
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt32(reader io.Reader) int32 {
	var result int32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint32(reader io.Reader) uint32 {
	var result uint32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readInt64(reader io.Reader) int64 {
	var result int64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readUint64(reader io.Reader) uint64 {
	var result uint64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat32(reader io.Reader) float32 {
	var result float32
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func readFloat64(reader io.Reader) float64 {
	var result float64
	if err := binary.Read(reader, binary.BigEndian, &result); err != nil {
		panic(err)
	}
	return result
}

func init() {

	uniffiCheckChecksums()
}

func uniffiCheckChecksums() {
	// Get the bindings contract version from our ComponentInterface
	bindingsContractVersion := 30
	// Get the scaffolding contract version by calling the into the dylib
	scaffoldingContractVersion := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint32_t {
		return C.ffi_computeruse_uniffi_contract_version()
	})
	if bindingsContractVersion != int(scaffoldingContractVersion) {
		// If this happens try cleaning and rebuilding your project
		panic("computeruse: UniFFI contract version mismatch")
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_func_sdk_version()
		})
		if checksum != 29787 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_func_sdk_version: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_access_token()
		})
		if checksum != 43213 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_access_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_base_url()
		})
		if checksum != 3049 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_base_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_create_session()
		})
		if checksum != 53219 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_create_session: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_evaluate_policy()
		})
		if checksum != 58167 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_evaluate_policy: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_get_session()
		})
		if checksum != 41831 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_get_session: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_list_sessions()
		})
		if checksum != 35950 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_list_sessions: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_me()
		})
		if checksum != 38631 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_me: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_policy_presets()
		})
		if checksum != 38635 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_policy_presets: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_session()
		})
		if checksum != 4961 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_session: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_client_validate_policy()
		})
		if checksum != 56092 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_client_validate_policy: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_call_tool()
		})
		if checksum != 56012 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_call_tool: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_delete()
		})
		if checksum != 2844 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_delete: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_id()
		})
		if checksum != 1449 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_id: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_last_info()
		})
		if checksum != 11531 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_last_info: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_list_tools()
		})
		if checksum != 44195 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_list_tools: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_mcp_url()
		})
		if checksum != 31882 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_mcp_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_policy()
		})
		if checksum != 39885 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_policy: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_put_policy()
		})
		if checksum != 50507 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_put_policy: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_refresh()
		})
		if checksum != 57459 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_refresh: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_rename()
		})
		if checksum != 21385 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_rename: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_reset_policy()
		})
		if checksum != 30937 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_reset_policy: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_resume()
		})
		if checksum != 38912 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_resume: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_run_js()
		})
		if checksum != 32217 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_run_js: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_run_js_with()
		})
		if checksum != 36921 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_run_js_with: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_set_policy_management()
		})
		if checksum != 53834 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_set_policy_management: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_sleep()
		})
		if checksum != 29786 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_sleep: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_stop()
		})
		if checksum != 28337 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_stop: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_wait_until()
		})
		if checksum != 20279 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_wait_until: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_wait_until_running()
		})
		if checksum != 12070 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_wait_until_running: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_session_wake()
		})
		if checksum != 61272 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_session_wake: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_api_token()
		})
		if checksum != 29878 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_api_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_base_url()
		})
		if checksum != 4181 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_base_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_build()
		})
		if checksum != 51626 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_build: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_exchange_token()
		})
		if checksum != 48566 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_exchange_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_max_retries()
		})
		if checksum != 21558 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_max_retries: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_mcp_timeout_ms()
		})
		if checksum != 23421 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_mcp_timeout_ms: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_retry_base_delay_ms()
		})
		if checksum != 64230 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_retry_base_delay_ms: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_scopes()
		})
		if checksum != 47492 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_scopes: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_timeout_ms()
		})
		if checksum != 31730 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_timeout_ms: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_user_agent()
		})
		if checksum != 41417 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_user_agent: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_clientoptionsbuilder_wake_timeout_ms()
		})
		if checksum != 12944 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_clientoptionsbuilder_wake_timeout_ms: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_createsessionrequestbuilder_build()
		})
		if checksum != 55341 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_createsessionrequestbuilder_build: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_createsessionrequestbuilder_name()
		})
		if checksum != 4620 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_createsessionrequestbuilder_name: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_createsessionrequestbuilder_policy()
		})
		if checksum != 22970 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_createsessionrequestbuilder_policy: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_createsessionrequestbuilder_policy_preset()
		})
		if checksum != 10001 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_createsessionrequestbuilder_policy_preset: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_managementbuilder_build()
		})
		if checksum != 28083 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_managementbuilder_build: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_managementbuilder_managed_url()
		})
		if checksum != 24282 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_managementbuilder_managed_url: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_managementbuilder_mode()
		})
		if checksum != 39678 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_managementbuilder_mode: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_policyinputbuilder_build()
		})
		if checksum != 12359 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_policyinputbuilder_build: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_policyinputbuilder_if_match_version()
		})
		if checksum != 351 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_policyinputbuilder_if_match_version: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_policyinputbuilder_management()
		})
		if checksum != 32525 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_policyinputbuilder_management: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_policyinputbuilder_source()
		})
		if checksum != 48322 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_policyinputbuilder_source: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_runjsrequestbuilder_build()
		})
		if checksum != 5467 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_runjsrequestbuilder_build: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_runjsrequestbuilder_code()
		})
		if checksum != 63848 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_runjsrequestbuilder_code: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_runjsrequestbuilder_execution_timeout_secs()
		})
		if checksum != 11649 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_runjsrequestbuilder_execution_timeout_secs: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_method_runjsrequestbuilder_heap_memory_max_mb()
		})
		if checksum != 54684 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_method_runjsrequestbuilder_heap_memory_max_mb: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_constructor_client_new()
		})
		if checksum != 57216 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_constructor_client_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_constructor_client_with_token()
		})
		if checksum != 19158 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_constructor_client_with_token: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_constructor_clientoptionsbuilder_new()
		})
		if checksum != 3601 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_constructor_clientoptionsbuilder_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_constructor_createsessionrequestbuilder_new()
		})
		if checksum != 18502 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_constructor_createsessionrequestbuilder_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_constructor_managementbuilder_new()
		})
		if checksum != 51554 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_constructor_managementbuilder_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_constructor_policyinputbuilder_new()
		})
		if checksum != 45688 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_constructor_policyinputbuilder_new: UniFFI API checksum mismatch")
		}
	}
	{
		checksum := rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint16_t {
			return C.uniffi_computeruse_checksum_constructor_runjsrequestbuilder_new()
		})
		if checksum != 18231 {
			// If this happens try cleaning and rebuilding your project
			panic("computeruse: uniffi_computeruse_checksum_constructor_runjsrequestbuilder_new: UniFFI API checksum mismatch")
		}
	}
}

type FfiConverterUint16 struct{}

var FfiConverterUint16INSTANCE = FfiConverterUint16{}

func (FfiConverterUint16) Lower(value uint16) C.uint16_t {
	return C.uint16_t(value)
}

func (FfiConverterUint16) Write(writer io.Writer, value uint16) {
	writeUint16(writer, value)
}

func (FfiConverterUint16) Lift(value C.uint16_t) uint16 {
	return uint16(value)
}

func (FfiConverterUint16) Read(reader io.Reader) uint16 {
	return readUint16(reader)
}

type FfiDestroyerUint16 struct{}

func (FfiDestroyerUint16) Destroy(_ uint16) {}

type FfiConverterUint32 struct{}

var FfiConverterUint32INSTANCE = FfiConverterUint32{}

func (FfiConverterUint32) Lower(value uint32) C.uint32_t {
	return C.uint32_t(value)
}

func (FfiConverterUint32) Write(writer io.Writer, value uint32) {
	writeUint32(writer, value)
}

func (FfiConverterUint32) Lift(value C.uint32_t) uint32 {
	return uint32(value)
}

func (FfiConverterUint32) Read(reader io.Reader) uint32 {
	return readUint32(reader)
}

type FfiDestroyerUint32 struct{}

func (FfiDestroyerUint32) Destroy(_ uint32) {}

type FfiConverterUint64 struct{}

var FfiConverterUint64INSTANCE = FfiConverterUint64{}

func (FfiConverterUint64) Lower(value uint64) C.uint64_t {
	return C.uint64_t(value)
}

func (FfiConverterUint64) Write(writer io.Writer, value uint64) {
	writeUint64(writer, value)
}

func (FfiConverterUint64) Lift(value C.uint64_t) uint64 {
	return uint64(value)
}

func (FfiConverterUint64) Read(reader io.Reader) uint64 {
	return readUint64(reader)
}

type FfiDestroyerUint64 struct{}

func (FfiDestroyerUint64) Destroy(_ uint64) {}

type FfiConverterInt64 struct{}

var FfiConverterInt64INSTANCE = FfiConverterInt64{}

func (FfiConverterInt64) Lower(value int64) C.int64_t {
	return C.int64_t(value)
}

func (FfiConverterInt64) Write(writer io.Writer, value int64) {
	writeInt64(writer, value)
}

func (FfiConverterInt64) Lift(value C.int64_t) int64 {
	return int64(value)
}

func (FfiConverterInt64) Read(reader io.Reader) int64 {
	return readInt64(reader)
}

type FfiDestroyerInt64 struct{}

func (FfiDestroyerInt64) Destroy(_ int64) {}

type FfiConverterBool struct{}

var FfiConverterBoolINSTANCE = FfiConverterBool{}

func (FfiConverterBool) Lower(value bool) C.int8_t {
	if value {
		return C.int8_t(1)
	}
	return C.int8_t(0)
}

func (FfiConverterBool) Write(writer io.Writer, value bool) {
	if value {
		writeInt8(writer, 1)
	} else {
		writeInt8(writer, 0)
	}
}

func (FfiConverterBool) Lift(value C.int8_t) bool {
	return value != 0
}

func (FfiConverterBool) Read(reader io.Reader) bool {
	return readInt8(reader) != 0
}

type FfiDestroyerBool struct{}

func (FfiDestroyerBool) Destroy(_ bool) {}

type FfiConverterString struct{}

var FfiConverterStringINSTANCE = FfiConverterString{}

func (FfiConverterString) Lift(rb RustBufferI) string {
	defer rb.Free()
	reader := rb.AsReader()
	b, err := io.ReadAll(reader)
	if err != nil {
		panic(fmt.Errorf("reading reader: %w", err))
	}
	return string(b)
}

func (FfiConverterString) Read(reader io.Reader) string {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading string, expected %d, read %d", length, read_length))
	}
	return string(buffer)
}

func (FfiConverterString) Lower(value string) C.RustBuffer {
	return stringToRustBuffer(value)
}

func (c FfiConverterString) LowerExternal(value string) ExternalCRustBuffer {
	return RustBufferFromC(stringToRustBuffer(value))
}

func (FfiConverterString) Write(writer io.Writer, value string) {
	if len(value) > math.MaxInt32 {
		panic("String is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := io.WriteString(writer, value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing string, expected %d, written %d", len(value), write_length))
	}
}

type FfiDestroyerString struct{}

func (FfiDestroyerString) Destroy(_ string) {}

type FfiConverterBytes struct{}

var FfiConverterBytesINSTANCE = FfiConverterBytes{}

func (c FfiConverterBytes) Lower(value []byte) C.RustBuffer {
	return LowerIntoRustBuffer[[]byte](c, value)
}

func (c FfiConverterBytes) LowerExternal(value []byte) ExternalCRustBuffer {
	return RustBufferFromC(c.Lower(value))
}

func (c FfiConverterBytes) Write(writer io.Writer, value []byte) {
	if len(value) > math.MaxInt32 {
		panic("[]byte is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	write_length, err := writer.Write(value)
	if err != nil {
		panic(err)
	}
	if write_length != len(value) {
		panic(fmt.Errorf("bad write length when writing []byte, expected %d, written %d", len(value), write_length))
	}
}

func (c FfiConverterBytes) Lift(rb RustBufferI) []byte {
	return LiftFromRustBuffer[[]byte](c, rb)
}

func (c FfiConverterBytes) Read(reader io.Reader) []byte {
	length := readInt32(reader)
	buffer := make([]byte, length)
	read_length, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic(err)
	}
	if read_length != int(length) {
		panic(fmt.Errorf("bad read length when reading []byte, expected %d, read %d", length, read_length))
	}
	return buffer
}

type FfiDestroyerBytes struct{}

func (FfiDestroyerBytes) Destroy(_ []byte) {}

// Below is an implementation of synchronization requirements outlined in the link.
// https://github.com/mozilla/uniffi-rs/blob/0dc031132d9493ca812c3af6e7dd60ad2ea95bf0/uniffi_bindgen/src/bindings/kotlin/templates/ObjectRuntime.kt#L31

type FfiObject struct {
	handle        C.uint64_t
	callCounter   atomic.Int64
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t
	freeFunction  func(C.uint64_t, *C.RustCallStatus)
	destroyed     atomic.Bool
}

func newFfiObject(
	handle C.uint64_t,
	cloneFunction func(C.uint64_t, *C.RustCallStatus) C.uint64_t,
	freeFunction func(C.uint64_t, *C.RustCallStatus),
) FfiObject {
	return FfiObject{
		handle:        handle,
		cloneFunction: cloneFunction,
		freeFunction:  freeFunction,
	}
}

func (ffiObject *FfiObject) incrementPointer(debugName string) C.uint64_t {
	for {
		counter := ffiObject.callCounter.Load()
		if counter <= -1 {
			panic(fmt.Errorf("%v object has already been destroyed", debugName))
		}
		if counter == math.MaxInt64 {
			panic(fmt.Errorf("%v object call counter would overflow", debugName))
		}
		if ffiObject.callCounter.CompareAndSwap(counter, counter+1) {
			break
		}
	}

	return rustCall(func(status *C.RustCallStatus) C.uint64_t {
		return ffiObject.cloneFunction(ffiObject.handle, status)
	})
}

func (ffiObject *FfiObject) decrementPointer() {
	if ffiObject.callCounter.Add(-1) == -1 {
		ffiObject.freeRustArcPtr()
	}
}

func (ffiObject *FfiObject) destroy() {
	if ffiObject.destroyed.CompareAndSwap(false, true) {
		if ffiObject.callCounter.Add(-1) == -1 {
			ffiObject.freeRustArcPtr()
		}
	}
}

func (ffiObject *FfiObject) freeRustArcPtr() {
	if ffiObject.handle == 0 {
		return
	}
	rustCall(func(status *C.RustCallStatus) int32 {
		ffiObject.freeFunction(ffiObject.handle, status)
		return 0
	})
}

// A client for the Computer Use API with one token.
//
// Cheap to share: clone the `Arc`. Its `Debug` does not print the token.
type ClientInterface interface {
	// The bearer token the next request would carry, for a caller that
	// opens its own connection (an MCP client of its own, say). It is the
	// short-lived access token unless `exchange_token` is off.
	// `force_refresh` asks for a new one. Treat the value as a secret.
	AccessToken(forceRefresh bool) (string, error)
	// The API host this client talks to, without a trailing slash.
	BaseUrl() string
	// Creates a session. Scope `sessions:write`.
	//
	// The answer does not wait for the desktop: the session is `starting`.
	// An MCP call waits for it; so does
	// [`Session::wait_until_running`].
	//
	// `409` ([`ComputerUseError::Conflict`]) at the session limit, `422`
	// ([`ComputerUseError::InvalidPolicy`]) for a policy that does not
	// validate, in which case nothing was created.
	CreateSession(request CreateSessionRequest) (*Session, error)
	// Asks a Rego policy about one sample call. `input_json` is the input
	// document, as JSON. Needs no scope.
	EvaluatePolicy(source string, inputJson string) (Evaluation, error)
	// Reads a session and returns a handle to it. Scope `sessions:read`.
	// `404` for one that does not exist or is not the caller's.
	GetSession(id string) (*Session, error)
	// The caller's sessions. Scope `sessions:read`; refused (403) for a
	// token bound to one session.
	ListSessions() ([]SessionInfo, error)
	// Who the token acts as. Needs no scope.
	Me() (Me, error)
	// The ready-made policies, `unrestricted` first. Needs no scope.
	PolicyPresets() ([]PolicyPreset, error)
	// A handle to the session `id`, without asking the API whether it
	// exists. Enough for a token with `sessions:connect` alone.
	Session(id string) (*Session, error)
	// Checks a Rego policy without saving it. An invalid policy is not an
	// error here: it is a [`Validation`] whose `ok` is false. Needs no
	// scope.
	ValidatePolicy(source string) (Validation, error)
}

// A client for the Computer Use API with one token.
//
// Cheap to share: clone the `Arc`. Its `Debug` does not print the token.
type Client struct {
	ffiObject FfiObject
}

// A client made from `options`. Nothing is sent until the first call.
func NewClient(options ClientOptions) (*Client, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*ComputerUseError](FfiConverterComputerUseError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_constructor_client_new(FfiConverterClientOptionsINSTANCE.Lower(options), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Client
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterClientINSTANCE.Lift(_uniffiRV), nil
	}
}

// A client for the hosted service with every default.
func ClientWithToken(apiToken string) (*Client, error) {
	_uniffiRV, _uniffiErr := rustCallWithError[*ComputerUseError](FfiConverterComputerUseError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_constructor_client_with_token(FfiConverterStringINSTANCE.Lower(apiToken), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Client
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterClientINSTANCE.Lift(_uniffiRV), nil
	}
}

// The bearer token the next request would carry, for a caller that
// opens its own connection (an MCP client of its own, say). It is the
// short-lived access token unless `exchange_token` is off.
// `force_refresh` asks for a new one. Treat the value as a secret.
func (_self *Client) AccessToken(forceRefresh bool) (string, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) string {
			return FfiConverterStringINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_client_access_token(
			_pointer, FfiConverterBoolINSTANCE.Lower(forceRefresh)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// The API host this client talks to, without a trailing slash.
func (_self *Client) BaseUrl() string {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_client_base_url(
				_pointer, _uniffiStatus),
		}
	}))
}

// Creates a session. Scope `sessions:write`.
//
// The answer does not wait for the desktop: the session is `starting`.
// An MCP call waits for it; so does
// [`Session::wait_until_running`].
//
// `409` ([`ComputerUseError::Conflict`]) at the session limit, `422`
// ([`ComputerUseError::InvalidPolicy`]) for a policy that does not
// validate, in which case nothing was created.
func (_self *Client) CreateSession(request CreateSessionRequest) (*Session, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_computeruse_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *Session {
			return FfiConverterSessionINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_client_create_session(
			_pointer, FfiConverterCreateSessionRequestINSTANCE.Lower(request)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Asks a Rego policy about one sample call. `input_json` is the input
// document, as JSON. Needs no scope.
func (_self *Client) EvaluatePolicy(source string, inputJson string) (Evaluation, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Evaluation {
			return FfiConverterEvaluationINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_client_evaluate_policy(
			_pointer, FfiConverterStringINSTANCE.Lower(source), FfiConverterStringINSTANCE.Lower(inputJson)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Reads a session and returns a handle to it. Scope `sessions:read`.
// `404` for one that does not exist or is not the caller's.
func (_self *Client) GetSession(id string) (*Session, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
			res := C.ffi_computeruse_rust_future_complete_u64(handle, status)
			return res
		},
		// liftFn
		func(ffi C.uint64_t) *Session {
			return FfiConverterSessionINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_client_get_session(
			_pointer, FfiConverterStringINSTANCE.Lower(id)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_u64(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_u64(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// The caller's sessions. Scope `sessions:read`; refused (403) for a
// token bound to one session.
func (_self *Client) ListSessions() ([]SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []SessionInfo {
			return FfiConverterSequenceSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_client_list_sessions(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Who the token acts as. Needs no scope.
func (_self *Client) Me() (Me, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Me {
			return FfiConverterMeINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_client_me(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// The ready-made policies, `unrestricted` first. Needs no scope.
func (_self *Client) PolicyPresets() ([]PolicyPreset, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []PolicyPreset {
			return FfiConverterSequencePolicyPresetINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_client_policy_presets(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// A handle to the session `id`, without asking the API whether it
// exists. Enough for a token with `sessions:connect` alone.
func (_self *Client) Session(id string) (*Session, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*ComputerUseError](FfiConverterComputerUseError{}, func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_client_session(
			_pointer, FfiConverterStringINSTANCE.Lower(id), _uniffiStatus)
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue *Session
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterSessionINSTANCE.Lift(_uniffiRV), nil
	}
}

// Checks a Rego policy without saving it. An invalid policy is not an
// error here: it is a [`Validation`] whose `ok` is false. Needs no
// scope.
func (_self *Client) ValidatePolicy(source string) (Validation, error) {
	_pointer := _self.ffiObject.incrementPointer("*Client")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Validation {
			return FfiConverterValidationINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_client_validate_policy(
			_pointer, FfiConverterStringINSTANCE.Lower(source)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *Client) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterClient struct{}

var FfiConverterClientINSTANCE = FfiConverterClient{}

func (c FfiConverterClient) Lift(handle C.uint64_t) *Client {
	result := &Client{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_computeruse_fn_clone_client(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_computeruse_fn_free_client(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Client).Destroy)
	return result
}

func (c FfiConverterClient) Read(reader io.Reader) *Client {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterClient) Lower(value *Client) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*Client")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterClient) Write(writer io.Writer, value *Client) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalClient(handle uint64) *Client {
	return FfiConverterClientINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalClient(value *Client) uint64 {
	return uint64(FfiConverterClientINSTANCE.Lower(value))
}

type FfiDestroyerClient struct{}

func (_ FfiDestroyerClient) Destroy(value *Client) {
	value.Destroy()
}

// Builds a [`ClientOptions`]. Each setter returns a new builder; the receiver is unchanged.
type ClientOptionsBuilderInterface interface {
	// An API token (`bjs_<id>_<secret>`), or an access token made from one.
	ApiToken(value string) *ClientOptionsBuilder
	// The API host, without a path. Default: `https://api.computeruse.site`.
	// `http` is accepted for a loopback address only.
	BaseUrl(value string) *ClientOptionsBuilder
	// The [`ClientOptions`], or `MissingRequiredField` naming the first required field left out.
	Build() (ClientOptions, error)
	// Exchange the API token for a short-lived access token and send that
	// (the default), or send the API token itself on every request.
	ExchangeToken(value bool) *ClientOptionsBuilder
	// How many times a request is tried again after `429`, `502`, `503`,
	// `504` or a connection that could not be made. Default 3.
	MaxRetries(value uint32) *ClientOptionsBuilder
	// The time limit of one MCP request, in milliseconds. Default 630 000:
	// the service holds a call for up to 300 seconds while a session
	// wakes, and `run_js` may then run for 300 more.
	McpTimeoutMs(value uint64) *ClientOptionsBuilder
	// The first pause between tries, in milliseconds; it doubles each
	// time. Default 500. A `Retry-After` the API sends is used instead.
	RetryBaseDelayMs(value uint64) *ClientOptionsBuilder
	// Narrows the access token to these scopes (a subset of the API
	// token's). Default: all of the API token's.
	Scopes(value []string) *ClientOptionsBuilder
	// The time limit of one API request, in milliseconds. Default 60 000.
	TimeoutMs(value uint64) *ClientOptionsBuilder
	// Added in front of the SDK's own `User-Agent`.
	UserAgent(value string) *ClientOptionsBuilder
	// How long an MCP call keeps trying while the session is waking
	// (`504` with `Retry-After`), in milliseconds. Default 600 000.
	WakeTimeoutMs(value uint64) *ClientOptionsBuilder
}

// Builds a [`ClientOptions`]. Each setter returns a new builder; the receiver is unchanged.
type ClientOptionsBuilder struct {
	ffiObject FfiObject
}

// A builder with nothing set.
func NewClientOptionsBuilder() *ClientOptionsBuilder {
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_constructor_clientoptionsbuilder_new(_uniffiStatus)
	}))
}

// An API token (`bjs_<id>_<secret>`), or an access token made from one.
func (_self *ClientOptionsBuilder) ApiToken(value string) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_api_token(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// The API host, without a path. Default: `https://api.computeruse.site`.
// `http` is accepted for a loopback address only.
func (_self *ClientOptionsBuilder) BaseUrl(value string) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_base_url(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// The [`ClientOptions`], or `MissingRequiredField` naming the first required field left out.
func (_self *ClientOptionsBuilder) Build() (ClientOptions, error) {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*BuildError](FfiConverterBuildError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_clientoptionsbuilder_build(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue ClientOptions
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterClientOptionsINSTANCE.Lift(_uniffiRV), nil
	}
}

// Exchange the API token for a short-lived access token and send that
// (the default), or send the API token itself on every request.
func (_self *ClientOptionsBuilder) ExchangeToken(value bool) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_exchange_token(
			_pointer, FfiConverterBoolINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// How many times a request is tried again after `429`, `502`, `503`,
// `504` or a connection that could not be made. Default 3.
func (_self *ClientOptionsBuilder) MaxRetries(value uint32) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_max_retries(
			_pointer, FfiConverterUint32INSTANCE.Lower(value), _uniffiStatus)
	}))
}

// The time limit of one MCP request, in milliseconds. Default 630 000:
// the service holds a call for up to 300 seconds while a session
// wakes, and `run_js` may then run for 300 more.
func (_self *ClientOptionsBuilder) McpTimeoutMs(value uint64) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_mcp_timeout_ms(
			_pointer, FfiConverterUint64INSTANCE.Lower(value), _uniffiStatus)
	}))
}

// The first pause between tries, in milliseconds; it doubles each
// time. Default 500. A `Retry-After` the API sends is used instead.
func (_self *ClientOptionsBuilder) RetryBaseDelayMs(value uint64) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_retry_base_delay_ms(
			_pointer, FfiConverterUint64INSTANCE.Lower(value), _uniffiStatus)
	}))
}

// Narrows the access token to these scopes (a subset of the API
// token's). Default: all of the API token's.
func (_self *ClientOptionsBuilder) Scopes(value []string) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_scopes(
			_pointer, FfiConverterSequenceStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// The time limit of one API request, in milliseconds. Default 60 000.
func (_self *ClientOptionsBuilder) TimeoutMs(value uint64) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_timeout_ms(
			_pointer, FfiConverterUint64INSTANCE.Lower(value), _uniffiStatus)
	}))
}

// Added in front of the SDK's own `User-Agent`.
func (_self *ClientOptionsBuilder) UserAgent(value string) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_user_agent(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// How long an MCP call keeps trying while the session is waking
// (`504` with `Retry-After`), in milliseconds. Default 600 000.
func (_self *ClientOptionsBuilder) WakeTimeoutMs(value uint64) *ClientOptionsBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_clientoptionsbuilder_wake_timeout_ms(
			_pointer, FfiConverterUint64INSTANCE.Lower(value), _uniffiStatus)
	}))
}
func (object *ClientOptionsBuilder) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterClientOptionsBuilder struct{}

var FfiConverterClientOptionsBuilderINSTANCE = FfiConverterClientOptionsBuilder{}

func (c FfiConverterClientOptionsBuilder) Lift(handle C.uint64_t) *ClientOptionsBuilder {
	result := &ClientOptionsBuilder{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_computeruse_fn_clone_clientoptionsbuilder(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_computeruse_fn_free_clientoptionsbuilder(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*ClientOptionsBuilder).Destroy)
	return result
}

func (c FfiConverterClientOptionsBuilder) Read(reader io.Reader) *ClientOptionsBuilder {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterClientOptionsBuilder) Lower(value *ClientOptionsBuilder) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*ClientOptionsBuilder")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterClientOptionsBuilder) Write(writer io.Writer, value *ClientOptionsBuilder) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalClientOptionsBuilder(handle uint64) *ClientOptionsBuilder {
	return FfiConverterClientOptionsBuilderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalClientOptionsBuilder(value *ClientOptionsBuilder) uint64 {
	return uint64(FfiConverterClientOptionsBuilderINSTANCE.Lower(value))
}

type FfiDestroyerClientOptionsBuilder struct{}

func (_ FfiDestroyerClientOptionsBuilder) Destroy(value *ClientOptionsBuilder) {
	value.Destroy()
}

// Builds a [`CreateSessionRequest`]. Each setter returns a new builder; the receiver is unchanged.
type CreateSessionRequestBuilderInterface interface {
	// The [`CreateSessionRequest`], or `MissingRequiredField` naming the first required field left out.
	Build() (CreateSessionRequest, error)
	// 1 to 63 characters. Left out, the service picks one like
	// `brave-otter`.
	Name(value string) *CreateSessionRequestBuilder
	// The session's policy. Left out, with no preset: unrestricted.
	Policy(value PolicyInput) *CreateSessionRequestBuilder
	// The id of a preset (`GET /v1/policy-presets`) to use as the policy.
	// The SDK reads the preset and sends its source. Not with `policy`.
	PolicyPreset(value string) *CreateSessionRequestBuilder
}

// Builds a [`CreateSessionRequest`]. Each setter returns a new builder; the receiver is unchanged.
type CreateSessionRequestBuilder struct {
	ffiObject FfiObject
}

// A builder with nothing set.
func NewCreateSessionRequestBuilder() *CreateSessionRequestBuilder {
	return FfiConverterCreateSessionRequestBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_constructor_createsessionrequestbuilder_new(_uniffiStatus)
	}))
}

// The [`CreateSessionRequest`], or `MissingRequiredField` naming the first required field left out.
func (_self *CreateSessionRequestBuilder) Build() (CreateSessionRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*CreateSessionRequestBuilder")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*BuildError](FfiConverterBuildError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_createsessionrequestbuilder_build(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue CreateSessionRequest
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterCreateSessionRequestINSTANCE.Lift(_uniffiRV), nil
	}
}

// 1 to 63 characters. Left out, the service picks one like
// `brave-otter`.
func (_self *CreateSessionRequestBuilder) Name(value string) *CreateSessionRequestBuilder {
	_pointer := _self.ffiObject.incrementPointer("*CreateSessionRequestBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterCreateSessionRequestBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_createsessionrequestbuilder_name(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// The session's policy. Left out, with no preset: unrestricted.
func (_self *CreateSessionRequestBuilder) Policy(value PolicyInput) *CreateSessionRequestBuilder {
	_pointer := _self.ffiObject.incrementPointer("*CreateSessionRequestBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterCreateSessionRequestBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_createsessionrequestbuilder_policy(
			_pointer, FfiConverterPolicyInputINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// The id of a preset (`GET /v1/policy-presets`) to use as the policy.
// The SDK reads the preset and sends its source. Not with `policy`.
func (_self *CreateSessionRequestBuilder) PolicyPreset(value string) *CreateSessionRequestBuilder {
	_pointer := _self.ffiObject.incrementPointer("*CreateSessionRequestBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterCreateSessionRequestBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_createsessionrequestbuilder_policy_preset(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}
func (object *CreateSessionRequestBuilder) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterCreateSessionRequestBuilder struct{}

var FfiConverterCreateSessionRequestBuilderINSTANCE = FfiConverterCreateSessionRequestBuilder{}

func (c FfiConverterCreateSessionRequestBuilder) Lift(handle C.uint64_t) *CreateSessionRequestBuilder {
	result := &CreateSessionRequestBuilder{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_computeruse_fn_clone_createsessionrequestbuilder(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_computeruse_fn_free_createsessionrequestbuilder(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*CreateSessionRequestBuilder).Destroy)
	return result
}

func (c FfiConverterCreateSessionRequestBuilder) Read(reader io.Reader) *CreateSessionRequestBuilder {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterCreateSessionRequestBuilder) Lower(value *CreateSessionRequestBuilder) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*CreateSessionRequestBuilder")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterCreateSessionRequestBuilder) Write(writer io.Writer, value *CreateSessionRequestBuilder) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalCreateSessionRequestBuilder(handle uint64) *CreateSessionRequestBuilder {
	return FfiConverterCreateSessionRequestBuilderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalCreateSessionRequestBuilder(value *CreateSessionRequestBuilder) uint64 {
	return uint64(FfiConverterCreateSessionRequestBuilderINSTANCE.Lower(value))
}

type FfiDestroyerCreateSessionRequestBuilder struct{}

func (_ FfiDestroyerCreateSessionRequestBuilder) Destroy(value *CreateSessionRequestBuilder) {
	value.Destroy()
}

// Builds a [`Management`]. Each setter returns a new builder; the receiver is unchanged.
type ManagementBuilderInterface interface {
	// The [`Management`], or `MissingRequiredField` naming the first required field left out.
	Build() (Management, error)
	// Required, and https, when `mode` is `Iac`: where the policy's source
	// is kept. The app links to it.
	ManagedUrl(value string) *ManagementBuilder
	Mode(value ManagementMode) *ManagementBuilder
}

// Builds a [`Management`]. Each setter returns a new builder; the receiver is unchanged.
type ManagementBuilder struct {
	ffiObject FfiObject
}

// A builder with nothing set.
func NewManagementBuilder() *ManagementBuilder {
	return FfiConverterManagementBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_constructor_managementbuilder_new(_uniffiStatus)
	}))
}

// The [`Management`], or `MissingRequiredField` naming the first required field left out.
func (_self *ManagementBuilder) Build() (Management, error) {
	_pointer := _self.ffiObject.incrementPointer("*ManagementBuilder")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*BuildError](FfiConverterBuildError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_managementbuilder_build(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue Management
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterManagementINSTANCE.Lift(_uniffiRV), nil
	}
}

// Required, and https, when `mode` is `Iac`: where the policy's source
// is kept. The app links to it.
func (_self *ManagementBuilder) ManagedUrl(value string) *ManagementBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ManagementBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterManagementBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_managementbuilder_managed_url(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}

func (_self *ManagementBuilder) Mode(value ManagementMode) *ManagementBuilder {
	_pointer := _self.ffiObject.incrementPointer("*ManagementBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterManagementBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_managementbuilder_mode(
			_pointer, FfiConverterManagementModeINSTANCE.Lower(value), _uniffiStatus)
	}))
}
func (object *ManagementBuilder) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterManagementBuilder struct{}

var FfiConverterManagementBuilderINSTANCE = FfiConverterManagementBuilder{}

func (c FfiConverterManagementBuilder) Lift(handle C.uint64_t) *ManagementBuilder {
	result := &ManagementBuilder{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_computeruse_fn_clone_managementbuilder(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_computeruse_fn_free_managementbuilder(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*ManagementBuilder).Destroy)
	return result
}

func (c FfiConverterManagementBuilder) Read(reader io.Reader) *ManagementBuilder {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterManagementBuilder) Lower(value *ManagementBuilder) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*ManagementBuilder")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterManagementBuilder) Write(writer io.Writer, value *ManagementBuilder) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalManagementBuilder(handle uint64) *ManagementBuilder {
	return FfiConverterManagementBuilderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalManagementBuilder(value *ManagementBuilder) uint64 {
	return uint64(FfiConverterManagementBuilderINSTANCE.Lower(value))
}

type FfiDestroyerManagementBuilder struct{}

func (_ FfiDestroyerManagementBuilder) Destroy(value *ManagementBuilder) {
	value.Destroy()
}

// Builds a [`PolicyInput`]. Each setter returns a new builder; the receiver is unchanged.
type PolicyInputBuilderInterface interface {
	// The [`PolicyInput`], or `MissingRequiredField` naming the first required field left out.
	Build() (PolicyInput, error)
	// Write only if the policy is still at this version (`If-Match`).
	IfMatchVersion(value int64) *PolicyInputBuilder
	// A token may write a policy in `editor` mode only by moving it to
	// `iac` here.
	Management(value Management) *PolicyInputBuilder
	// The Rego module.
	Source(value string) *PolicyInputBuilder
}

// Builds a [`PolicyInput`]. Each setter returns a new builder; the receiver is unchanged.
type PolicyInputBuilder struct {
	ffiObject FfiObject
}

// A builder with nothing set.
func NewPolicyInputBuilder() *PolicyInputBuilder {
	return FfiConverterPolicyInputBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_constructor_policyinputbuilder_new(_uniffiStatus)
	}))
}

// The [`PolicyInput`], or `MissingRequiredField` naming the first required field left out.
func (_self *PolicyInputBuilder) Build() (PolicyInput, error) {
	_pointer := _self.ffiObject.incrementPointer("*PolicyInputBuilder")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*BuildError](FfiConverterBuildError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_policyinputbuilder_build(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue PolicyInput
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterPolicyInputINSTANCE.Lift(_uniffiRV), nil
	}
}

// Write only if the policy is still at this version (`If-Match`).
func (_self *PolicyInputBuilder) IfMatchVersion(value int64) *PolicyInputBuilder {
	_pointer := _self.ffiObject.incrementPointer("*PolicyInputBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterPolicyInputBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_policyinputbuilder_if_match_version(
			_pointer, FfiConverterInt64INSTANCE.Lower(value), _uniffiStatus)
	}))
}

// A token may write a policy in `editor` mode only by moving it to
// `iac` here.
func (_self *PolicyInputBuilder) Management(value Management) *PolicyInputBuilder {
	_pointer := _self.ffiObject.incrementPointer("*PolicyInputBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterPolicyInputBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_policyinputbuilder_management(
			_pointer, FfiConverterManagementINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// The Rego module.
func (_self *PolicyInputBuilder) Source(value string) *PolicyInputBuilder {
	_pointer := _self.ffiObject.incrementPointer("*PolicyInputBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterPolicyInputBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_policyinputbuilder_source(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}
func (object *PolicyInputBuilder) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterPolicyInputBuilder struct{}

var FfiConverterPolicyInputBuilderINSTANCE = FfiConverterPolicyInputBuilder{}

func (c FfiConverterPolicyInputBuilder) Lift(handle C.uint64_t) *PolicyInputBuilder {
	result := &PolicyInputBuilder{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_computeruse_fn_clone_policyinputbuilder(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_computeruse_fn_free_policyinputbuilder(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*PolicyInputBuilder).Destroy)
	return result
}

func (c FfiConverterPolicyInputBuilder) Read(reader io.Reader) *PolicyInputBuilder {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterPolicyInputBuilder) Lower(value *PolicyInputBuilder) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*PolicyInputBuilder")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterPolicyInputBuilder) Write(writer io.Writer, value *PolicyInputBuilder) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalPolicyInputBuilder(handle uint64) *PolicyInputBuilder {
	return FfiConverterPolicyInputBuilderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalPolicyInputBuilder(value *PolicyInputBuilder) uint64 {
	return uint64(FfiConverterPolicyInputBuilderINSTANCE.Lower(value))
}

type FfiDestroyerPolicyInputBuilder struct{}

func (_ FfiDestroyerPolicyInputBuilder) Destroy(value *PolicyInputBuilder) {
	value.Destroy()
}

// Builds a [`RunJsRequest`]. Each setter returns a new builder; the receiver is unchanged.
type RunJsRequestBuilderInterface interface {
	// The [`RunJsRequest`], or `MissingRequiredField` naming the first required field left out.
	Build() (RunJsRequest, error)
	// JavaScript or TypeScript. Top-level `await` works.
	Code(value string) *RunJsRequestBuilder
	// Time limit in seconds, 1 to 300. Default 30.
	ExecutionTimeoutSecs(value uint32) *RunJsRequestBuilder
	// Heap limit in MB. Minimum 4, default 8.
	HeapMemoryMaxMb(value uint32) *RunJsRequestBuilder
}

// Builds a [`RunJsRequest`]. Each setter returns a new builder; the receiver is unchanged.
type RunJsRequestBuilder struct {
	ffiObject FfiObject
}

// A builder with nothing set.
func NewRunJsRequestBuilder() *RunJsRequestBuilder {
	return FfiConverterRunJsRequestBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_constructor_runjsrequestbuilder_new(_uniffiStatus)
	}))
}

// The [`RunJsRequest`], or `MissingRequiredField` naming the first required field left out.
func (_self *RunJsRequestBuilder) Build() (RunJsRequest, error) {
	_pointer := _self.ffiObject.incrementPointer("*RunJsRequestBuilder")
	defer _self.ffiObject.decrementPointer()
	_uniffiRV, _uniffiErr := rustCallWithError[*BuildError](FfiConverterBuildError{}, func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_runjsrequestbuilder_build(
				_pointer, _uniffiStatus),
		}
	})
	if _uniffiErr != nil {
		var _uniffiDefaultValue RunJsRequest
		return _uniffiDefaultValue, _uniffiErr
	} else {
		return FfiConverterRunJsRequestINSTANCE.Lift(_uniffiRV), nil
	}
}

// JavaScript or TypeScript. Top-level `await` works.
func (_self *RunJsRequestBuilder) Code(value string) *RunJsRequestBuilder {
	_pointer := _self.ffiObject.incrementPointer("*RunJsRequestBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterRunJsRequestBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_runjsrequestbuilder_code(
			_pointer, FfiConverterStringINSTANCE.Lower(value), _uniffiStatus)
	}))
}

// Time limit in seconds, 1 to 300. Default 30.
func (_self *RunJsRequestBuilder) ExecutionTimeoutSecs(value uint32) *RunJsRequestBuilder {
	_pointer := _self.ffiObject.incrementPointer("*RunJsRequestBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterRunJsRequestBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_runjsrequestbuilder_execution_timeout_secs(
			_pointer, FfiConverterUint32INSTANCE.Lower(value), _uniffiStatus)
	}))
}

// Heap limit in MB. Minimum 4, default 8.
func (_self *RunJsRequestBuilder) HeapMemoryMaxMb(value uint32) *RunJsRequestBuilder {
	_pointer := _self.ffiObject.incrementPointer("*RunJsRequestBuilder")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterRunJsRequestBuilderINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) C.uint64_t {
		return C.uniffi_computeruse_fn_method_runjsrequestbuilder_heap_memory_max_mb(
			_pointer, FfiConverterUint32INSTANCE.Lower(value), _uniffiStatus)
	}))
}
func (object *RunJsRequestBuilder) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterRunJsRequestBuilder struct{}

var FfiConverterRunJsRequestBuilderINSTANCE = FfiConverterRunJsRequestBuilder{}

func (c FfiConverterRunJsRequestBuilder) Lift(handle C.uint64_t) *RunJsRequestBuilder {
	result := &RunJsRequestBuilder{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_computeruse_fn_clone_runjsrequestbuilder(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_computeruse_fn_free_runjsrequestbuilder(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*RunJsRequestBuilder).Destroy)
	return result
}

func (c FfiConverterRunJsRequestBuilder) Read(reader io.Reader) *RunJsRequestBuilder {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterRunJsRequestBuilder) Lower(value *RunJsRequestBuilder) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*RunJsRequestBuilder")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterRunJsRequestBuilder) Write(writer io.Writer, value *RunJsRequestBuilder) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalRunJsRequestBuilder(handle uint64) *RunJsRequestBuilder {
	return FfiConverterRunJsRequestBuilderINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalRunJsRequestBuilder(value *RunJsRequestBuilder) uint64 {
	return uint64(FfiConverterRunJsRequestBuilderINSTANCE.Lower(value))
}

type FfiDestroyerRunJsRequestBuilder struct{}

func (_ FfiDestroyerRunJsRequestBuilder) Destroy(value *RunJsRequestBuilder) {
	value.Destroy()
}

// A handle to one session: its lifecycle, its policy and its MCP endpoint.
//
// Made by [`Client::create_session`](crate::Client::create_session),
// [`Client::get_session`](crate::Client::get_session) or
// [`Client::session`](crate::Client::session).
type SessionInterface interface {
	// Calls any tool of the session's MCP server. `arguments_json` is the
	// tool's arguments as a JSON object; `None` for none. Scope
	// `sessions:connect`. A tool that reports failure is
	// [`ComputerUseError::Tool`].
	CallTool(name string, argumentsJson *string) (ToolResult, error)
	// Deletes the session, its disk and its snapshot. This cannot be
	// undone. Scope `sessions:write`. Deleting one that is already gone
	// succeeds.
	Delete() error
	// The session's id.
	Id() string
	// What the API last said about the session, without asking again.
	// `None` for a handle made by `Client::session` that has made no call
	// yet.
	LastInfo() *SessionInfo
	// The tools of the session's MCP server. Scope `sessions:connect`.
	ListTools() ([]ToolInfo, error)
	// The session's MCP endpoint on the API host, which takes a bearer
	// token: `https://api.<domain>/<id>/mcp`.
	McpUrl() string
	// The session's policy, with its source. Scope `policies:read`. `409`
	// for a session that predates policies.
	Policy() (Policy, error)
	// Replaces the policy. Scope `policies:write`.
	//
	// The answer's `state` is `ready` when the policy is in force
	// everywhere and `loading` when it is saved and still being loaded.
	// `422` ([`ComputerUseError::InvalidPolicy`]) saves nothing. `409`
	// when the policy is managed in the app's editor and `management`
	// does not move it to `iac`. `412` when `if_match_version` is not the
	// current version.
	PutPolicy(policy PolicyInput) (Policy, error)
	// Reads the session. Scope `sessions:read`.
	Refresh() (SessionInfo, error)
	// Renames the session: 1 to 63 characters. Scope `sessions:write`.
	Rename(name string) (SessionInfo, error)
	// Returns the policy to unrestricted, in `editor` mode. Scope
	// `policies:write`.
	ResetPolicy() (Policy, error)
	// Starts a stopped session from its disk, or wakes a sleeping one:
	// the `resume` action, which [`Session::wake`] is a route for. Scope
	// `sessions:write`. `402` where billing refuses it.
	Resume() (SessionInfo, error)
	// Runs JavaScript or TypeScript in the session with the `run_js` tool
	// and returns what it printed. Scope `sessions:connect`.
	//
	// The call wakes a sleeping session and waits for it. A stopped one
	// answers `409`. A program that throws is not an error of the call:
	// see [`RunJsResult::error`].
	RunJs(code string) (RunJsResult, error)
	// [`Session::run_js`] with a heap limit and a time limit.
	RunJsWith(request RunJsRequest) (RunJsResult, error)
	// Changes who manages the policy. Scope `policies:write`.
	SetPolicyManagement(management Management) (Policy, error)
	// Puts a running session to sleep now instead of after its idle time:
	// a snapshot of the running desktop is kept, compute is released, and
	// the next MCP call (or [`Session::wake`]) brings it back as it was.
	// Scope `sessions:write`.
	//
	// The answer comes when the snapshot is taken, which can take a
	// minute or two; the call allows three minutes whatever the client's
	// timeout. `state_saved` says whether there is a snapshot: without
	// one the session still sleeps, and wakes from its disk. A session
	// that is asleep already is left as it is. `409`
	// ([`ComputerUseError::Conflict`]) for one that is starting, stopping,
	// stopped or failed: only a running desktop has state to save.
	Sleep() (SessionInfo, error)
	// Stops the session: the desktop goes, the disk stays, no snapshot is
	// taken, and an MCP call does not wake it. Scope `sessions:write`.
	Stop() (SessionInfo, error)
	// Polls until the session is in `state`. `timeout_ms` defaults to five
	// minutes. Fails with [`ComputerUseError::SessionFailed`] if the
	// session goes to `failed` instead, and with
	// [`ComputerUseError::Timeout`] when the time runs out.
	WaitUntil(state SessionState, timeoutMs *uint64) (SessionInfo, error)
	// [`Session::wait_until`] for `running`.
	WaitUntilRunning(timeoutMs *uint64) (SessionInfo, error)
	// Starts a session that is asleep or stopped, without making an MCP
	// call: from its snapshot if it has one, from its disk otherwise. The
	// answer does not wait for the desktop; see
	// [`Session::wait_until_running`]. Waking one that is awake does
	// nothing. Scope `sessions:write`. `402` where billing refuses it.
	Wake() (SessionInfo, error)
}

// A handle to one session: its lifecycle, its policy and its MCP endpoint.
//
// Made by [`Client::create_session`](crate::Client::create_session),
// [`Client::get_session`](crate::Client::get_session) or
// [`Client::session`](crate::Client::session).
type Session struct {
	ffiObject FfiObject
}

// Calls any tool of the session's MCP server. `arguments_json` is the
// tool's arguments as a JSON object; `None` for none. Scope
// `sessions:connect`. A tool that reports failure is
// [`ComputerUseError::Tool`].
func (_self *Session) CallTool(name string, argumentsJson *string) (ToolResult, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) ToolResult {
			return FfiConverterToolResultINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_call_tool(
			_pointer, FfiConverterStringINSTANCE.Lower(name), FfiConverterOptionalStringINSTANCE.Lower(argumentsJson)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Deletes the session, its disk and its snapshot. This cannot be
// undone. Scope `sessions:write`. Deleting one that is already gone
// succeeds.
func (_self *Session) Delete() error {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	_, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) struct{} {
			C.ffi_computeruse_rust_future_complete_void(handle, status)
			return struct{}{}
		},
		// liftFn
		func(_ struct{}) struct{} { return struct{}{} },
		C.uniffi_computeruse_fn_method_session_delete(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_void(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_void(handle)
		},
	)

	if err == nil {
		return nil
	}

	return err
}

// The session's id.
func (_self *Session) Id() string {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_session_id(
				_pointer, _uniffiStatus),
		}
	}))
}

// What the API last said about the session, without asking again.
// `None` for a handle made by `Client::session` that has made no call
// yet.
func (_self *Session) LastInfo() *SessionInfo {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterOptionalSessionInfoINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_session_last_info(
				_pointer, _uniffiStatus),
		}
	}))
}

// The tools of the session's MCP server. Scope `sessions:connect`.
func (_self *Session) ListTools() ([]ToolInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) []ToolInfo {
			return FfiConverterSequenceToolInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_list_tools(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// The session's MCP endpoint on the API host, which takes a bearer
// token: `https://api.<domain>/<id>/mcp`.
func (_self *Session) McpUrl() string {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_method_session_mcp_url(
				_pointer, _uniffiStatus),
		}
	}))
}

// The session's policy, with its source. Scope `policies:read`. `409`
// for a session that predates policies.
func (_self *Session) Policy() (Policy, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Policy {
			return FfiConverterPolicyINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_policy(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Replaces the policy. Scope `policies:write`.
//
// The answer's `state` is `ready` when the policy is in force
// everywhere and `loading` when it is saved and still being loaded.
// `422` ([`ComputerUseError::InvalidPolicy`]) saves nothing. `409`
// when the policy is managed in the app's editor and `management`
// does not move it to `iac`. `412` when `if_match_version` is not the
// current version.
func (_self *Session) PutPolicy(policy PolicyInput) (Policy, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Policy {
			return FfiConverterPolicyINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_put_policy(
			_pointer, FfiConverterPolicyInputINSTANCE.Lower(policy)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Reads the session. Scope `sessions:read`.
func (_self *Session) Refresh() (SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionInfo {
			return FfiConverterSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_refresh(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Renames the session: 1 to 63 characters. Scope `sessions:write`.
func (_self *Session) Rename(name string) (SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionInfo {
			return FfiConverterSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_rename(
			_pointer, FfiConverterStringINSTANCE.Lower(name)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Returns the policy to unrestricted, in `editor` mode. Scope
// `policies:write`.
func (_self *Session) ResetPolicy() (Policy, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Policy {
			return FfiConverterPolicyINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_reset_policy(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Starts a stopped session from its disk, or wakes a sleeping one:
// the `resume` action, which [`Session::wake`] is a route for. Scope
// `sessions:write`. `402` where billing refuses it.
func (_self *Session) Resume() (SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionInfo {
			return FfiConverterSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_resume(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Runs JavaScript or TypeScript in the session with the `run_js` tool
// and returns what it printed. Scope `sessions:connect`.
//
// The call wakes a sleeping session and waits for it. A stopped one
// answers `409`. A program that throws is not an error of the call:
// see [`RunJsResult::error`].
func (_self *Session) RunJs(code string) (RunJsResult, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) RunJsResult {
			return FfiConverterRunJsResultINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_run_js(
			_pointer, FfiConverterStringINSTANCE.Lower(code)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// [`Session::run_js`] with a heap limit and a time limit.
func (_self *Session) RunJsWith(request RunJsRequest) (RunJsResult, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) RunJsResult {
			return FfiConverterRunJsResultINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_run_js_with(
			_pointer, FfiConverterRunJsRequestINSTANCE.Lower(request)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Changes who manages the policy. Scope `policies:write`.
func (_self *Session) SetPolicyManagement(management Management) (Policy, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) Policy {
			return FfiConverterPolicyINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_set_policy_management(
			_pointer, FfiConverterManagementINSTANCE.Lower(management)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Puts a running session to sleep now instead of after its idle time:
// a snapshot of the running desktop is kept, compute is released, and
// the next MCP call (or [`Session::wake`]) brings it back as it was.
// Scope `sessions:write`.
//
// The answer comes when the snapshot is taken, which can take a
// minute or two; the call allows three minutes whatever the client's
// timeout. `state_saved` says whether there is a snapshot: without
// one the session still sleeps, and wakes from its disk. A session
// that is asleep already is left as it is. `409`
// ([`ComputerUseError::Conflict`]) for one that is starting, stopping,
// stopped or failed: only a running desktop has state to save.
func (_self *Session) Sleep() (SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionInfo {
			return FfiConverterSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_sleep(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Stops the session: the desktop goes, the disk stays, no snapshot is
// taken, and an MCP call does not wake it. Scope `sessions:write`.
func (_self *Session) Stop() (SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionInfo {
			return FfiConverterSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_stop(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Polls until the session is in `state`. `timeout_ms` defaults to five
// minutes. Fails with [`ComputerUseError::SessionFailed`] if the
// session goes to `failed` instead, and with
// [`ComputerUseError::Timeout`] when the time runs out.
func (_self *Session) WaitUntil(state SessionState, timeoutMs *uint64) (SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionInfo {
			return FfiConverterSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_wait_until(
			_pointer, FfiConverterSessionStateINSTANCE.Lower(state), FfiConverterOptionalUint64INSTANCE.Lower(timeoutMs)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// [`Session::wait_until`] for `running`.
func (_self *Session) WaitUntilRunning(timeoutMs *uint64) (SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionInfo {
			return FfiConverterSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_wait_until_running(
			_pointer, FfiConverterOptionalUint64INSTANCE.Lower(timeoutMs)),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}

// Starts a session that is asleep or stopped, without making an MCP
// call: from its snapshot if it has one, from its disk otherwise. The
// answer does not wait for the desktop; see
// [`Session::wait_until_running`]. Waking one that is awake does
// nothing. Scope `sessions:write`. `402` where billing refuses it.
func (_self *Session) Wake() (SessionInfo, error) {
	_pointer := _self.ffiObject.incrementPointer("*Session")
	defer _self.ffiObject.decrementPointer()
	res, err := uniffiRustCallAsync[*ComputerUseError](
		FfiConverterComputerUseErrorINSTANCE,
		// completeFn
		func(handle C.uint64_t, status *C.RustCallStatus) RustBufferI {
			res := C.ffi_computeruse_rust_future_complete_rust_buffer(handle, status)
			return GoRustBuffer{
				inner: res,
			}
		},
		// liftFn
		func(ffi RustBufferI) SessionInfo {
			return FfiConverterSessionInfoINSTANCE.Lift(ffi)
		},
		C.uniffi_computeruse_fn_method_session_wake(
			_pointer),
		// pollFn
		func(handle C.uint64_t, continuation C.UniffiRustFutureContinuationCallback, data C.uint64_t) {
			C.ffi_computeruse_rust_future_poll_rust_buffer(handle, continuation, data)
		},
		// freeFn
		func(handle C.uint64_t) {
			C.ffi_computeruse_rust_future_free_rust_buffer(handle)
		},
	)

	if err == nil {
		return res, nil
	}

	return res, err
}
func (object *Session) Destroy() {
	runtime.SetFinalizer(object, nil)
	object.ffiObject.destroy()
}

type FfiConverterSession struct{}

var FfiConverterSessionINSTANCE = FfiConverterSession{}

func (c FfiConverterSession) Lift(handle C.uint64_t) *Session {
	result := &Session{
		newFfiObject(
			handle,
			func(handle C.uint64_t, status *C.RustCallStatus) C.uint64_t {
				return C.uniffi_computeruse_fn_clone_session(handle, status)
			},
			func(handle C.uint64_t, status *C.RustCallStatus) {
				C.uniffi_computeruse_fn_free_session(handle, status)
			},
		),
	}
	runtime.SetFinalizer(result, (*Session).Destroy)
	return result
}

func (c FfiConverterSession) Read(reader io.Reader) *Session {
	return c.Lift(C.uint64_t(readUint64(reader)))
}

func (c FfiConverterSession) Lower(value *Session) C.uint64_t {
	// TODO: this is bad - all synchronization from ObjectRuntime.go is discarded here,
	// because the handle will be decremented immediately after this function returns,
	// and someone will be left holding onto a non-locked handle.
	handle := value.ffiObject.incrementPointer("*Session")
	defer value.ffiObject.decrementPointer()
	return handle
}

func (c FfiConverterSession) Write(writer io.Writer, value *Session) {
	writeUint64(writer, uint64(c.Lower(value)))
}

func LiftFromExternalSession(handle uint64) *Session {
	return FfiConverterSessionINSTANCE.Lift(C.uint64_t(handle))
}

func LowerToExternalSession(value *Session) uint64 {
	return uint64(FfiConverterSessionINSTANCE.Lower(value))
}

type FfiDestroyerSession struct{}

func (_ FfiDestroyerSession) Destroy(value *Session) {
	value.Destroy()
}

// How a [`Client`](crate::Client) is made. Only `api_token` is required.
//
// Its `Debug` does not print the token.
type ClientOptions struct {
	// An API token (`bjs_<id>_<secret>`), or an access token made from one.
	ApiToken string
	// The API host, without a path. Default: `https://api.computeruse.site`.
	// `http` is accepted for a loopback address only.
	BaseUrl *string
	// Narrows the access token to these scopes (a subset of the API
	// token's). Default: all of the API token's.
	Scopes *[]string
	// Exchange the API token for a short-lived access token and send that
	// (the default), or send the API token itself on every request.
	ExchangeToken *bool
	// The time limit of one API request, in milliseconds. Default 60 000.
	TimeoutMs *uint64
	// The time limit of one MCP request, in milliseconds. Default 630 000:
	// the service holds a call for up to 300 seconds while a session
	// wakes, and `run_js` may then run for 300 more.
	McpTimeoutMs *uint64
	// How many times a request is tried again after `429`, `502`, `503`,
	// `504` or a connection that could not be made. Default 3.
	MaxRetries *uint32
	// The first pause between tries, in milliseconds; it doubles each
	// time. Default 500. A `Retry-After` the API sends is used instead.
	RetryBaseDelayMs *uint64
	// How long an MCP call keeps trying while the session is waking
	// (`504` with `Retry-After`), in milliseconds. Default 600 000.
	WakeTimeoutMs *uint64
	// Added in front of the SDK's own `User-Agent`.
	UserAgent *string
}

func (r *ClientOptions) Destroy() {
	FfiDestroyerString{}.Destroy(r.ApiToken)
	FfiDestroyerOptionalString{}.Destroy(r.BaseUrl)
	FfiDestroyerOptionalSequenceString{}.Destroy(r.Scopes)
	FfiDestroyerOptionalBool{}.Destroy(r.ExchangeToken)
	FfiDestroyerOptionalUint64{}.Destroy(r.TimeoutMs)
	FfiDestroyerOptionalUint64{}.Destroy(r.McpTimeoutMs)
	FfiDestroyerOptionalUint32{}.Destroy(r.MaxRetries)
	FfiDestroyerOptionalUint64{}.Destroy(r.RetryBaseDelayMs)
	FfiDestroyerOptionalUint64{}.Destroy(r.WakeTimeoutMs)
	FfiDestroyerOptionalString{}.Destroy(r.UserAgent)
}

type FfiConverterClientOptions struct{}

var FfiConverterClientOptionsINSTANCE = FfiConverterClientOptions{}

func (c FfiConverterClientOptions) Lift(rb RustBufferI) ClientOptions {
	return LiftFromRustBuffer[ClientOptions](c, rb)
}

func (c FfiConverterClientOptions) Read(reader io.Reader) ClientOptions {
	return ClientOptions{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalSequenceStringINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalUint64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterClientOptions) Lower(value ClientOptions) C.RustBuffer {
	return LowerIntoRustBuffer[ClientOptions](c, value)
}

func (c FfiConverterClientOptions) LowerExternal(value ClientOptions) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ClientOptions](c, value))
}

func (c FfiConverterClientOptions) Write(writer io.Writer, value ClientOptions) {
	FfiConverterStringINSTANCE.Write(writer, value.ApiToken)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.BaseUrl)
	FfiConverterOptionalSequenceStringINSTANCE.Write(writer, value.Scopes)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.ExchangeToken)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.TimeoutMs)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.McpTimeoutMs)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.MaxRetries)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.RetryBaseDelayMs)
	FfiConverterOptionalUint64INSTANCE.Write(writer, value.WakeTimeoutMs)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.UserAgent)
}

type FfiDestroyerClientOptions struct{}

func (_ FfiDestroyerClientOptions) Destroy(value ClientOptions) {
	value.Destroy()
}

// One block of a tool's result.
type ContentBlock struct {
	// `text`, `image`, `audio`, `resource`, ...
	Kind string
	// For `text`.
	Text *string
	// For `image` and `audio`: the decoded bytes.
	Data     *[]byte
	MimeType *string
	// The block as the server sent it.
	RawJson string
}

func (r *ContentBlock) Destroy() {
	FfiDestroyerString{}.Destroy(r.Kind)
	FfiDestroyerOptionalString{}.Destroy(r.Text)
	FfiDestroyerOptionalBytes{}.Destroy(r.Data)
	FfiDestroyerOptionalString{}.Destroy(r.MimeType)
	FfiDestroyerString{}.Destroy(r.RawJson)
}

type FfiConverterContentBlock struct{}

var FfiConverterContentBlockINSTANCE = FfiConverterContentBlock{}

func (c FfiConverterContentBlock) Lift(rb RustBufferI) ContentBlock {
	return LiftFromRustBuffer[ContentBlock](c, rb)
}

func (c FfiConverterContentBlock) Read(reader io.Reader) ContentBlock {
	return ContentBlock{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalBytesINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterContentBlock) Lower(value ContentBlock) C.RustBuffer {
	return LowerIntoRustBuffer[ContentBlock](c, value)
}

func (c FfiConverterContentBlock) LowerExternal(value ContentBlock) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ContentBlock](c, value))
}

func (c FfiConverterContentBlock) Write(writer io.Writer, value ContentBlock) {
	FfiConverterStringINSTANCE.Write(writer, value.Kind)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Text)
	FfiConverterOptionalBytesINSTANCE.Write(writer, value.Data)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.MimeType)
	FfiConverterStringINSTANCE.Write(writer, value.RawJson)
}

type FfiDestroyerContentBlock struct{}

func (_ FfiDestroyerContentBlock) Destroy(value ContentBlock) {
	value.Destroy()
}

// A session to create. Every field is optional.
type CreateSessionRequest struct {
	// 1 to 63 characters. Left out, the service picks one like
	// `brave-otter`.
	Name *string
	// The session's policy. Left out, with no preset: unrestricted.
	Policy *PolicyInput
	// The id of a preset (`GET /v1/policy-presets`) to use as the policy.
	// The SDK reads the preset and sends its source. Not with `policy`.
	PolicyPreset *string
}

func (r *CreateSessionRequest) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.Name)
	FfiDestroyerOptionalPolicyInput{}.Destroy(r.Policy)
	FfiDestroyerOptionalString{}.Destroy(r.PolicyPreset)
}

type FfiConverterCreateSessionRequest struct{}

var FfiConverterCreateSessionRequestINSTANCE = FfiConverterCreateSessionRequest{}

func (c FfiConverterCreateSessionRequest) Lift(rb RustBufferI) CreateSessionRequest {
	return LiftFromRustBuffer[CreateSessionRequest](c, rb)
}

func (c FfiConverterCreateSessionRequest) Read(reader io.Reader) CreateSessionRequest {
	return CreateSessionRequest{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalPolicyInputINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterCreateSessionRequest) Lower(value CreateSessionRequest) C.RustBuffer {
	return LowerIntoRustBuffer[CreateSessionRequest](c, value)
}

func (c FfiConverterCreateSessionRequest) LowerExternal(value CreateSessionRequest) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[CreateSessionRequest](c, value))
}

func (c FfiConverterCreateSessionRequest) Write(writer io.Writer, value CreateSessionRequest) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Name)
	FfiConverterOptionalPolicyInputINSTANCE.Write(writer, value.Policy)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.PolicyPreset)
}

type FfiDestroyerCreateSessionRequest struct{}

func (_ FfiDestroyerCreateSessionRequest) Destroy(value CreateSessionRequest) {
	value.Destroy()
}

// One error or warning about a policy's source. `row` and `col` are
// 1-based.
type Diagnostic struct {
	Row     *uint32
	Col     *uint32
	Code    string
	Message string
}

func (r *Diagnostic) Destroy() {
	FfiDestroyerOptionalUint32{}.Destroy(r.Row)
	FfiDestroyerOptionalUint32{}.Destroy(r.Col)
	FfiDestroyerString{}.Destroy(r.Code)
	FfiDestroyerString{}.Destroy(r.Message)
}

type FfiConverterDiagnostic struct{}

var FfiConverterDiagnosticINSTANCE = FfiConverterDiagnostic{}

func (c FfiConverterDiagnostic) Lift(rb RustBufferI) Diagnostic {
	return LiftFromRustBuffer[Diagnostic](c, rb)
}

func (c FfiConverterDiagnostic) Read(reader io.Reader) Diagnostic {
	return Diagnostic{
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterDiagnostic) Lower(value Diagnostic) C.RustBuffer {
	return LowerIntoRustBuffer[Diagnostic](c, value)
}

func (c FfiConverterDiagnostic) LowerExternal(value Diagnostic) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Diagnostic](c, value))
}

func (c FfiConverterDiagnostic) Write(writer io.Writer, value Diagnostic) {
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Row)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.Col)
	FfiConverterStringINSTANCE.Write(writer, value.Code)
	FfiConverterStringINSTANCE.Write(writer, value.Message)
}

type FfiDestroyerDiagnostic struct{}

func (_ FfiDestroyerDiagnostic) Destroy(value Diagnostic) {
	value.Destroy()
}

// What a policy decides about one sample call.
type Evaluation struct {
	// Whether the policy could be evaluated at all.
	Ok bool
	// The decision, when `ok`.
	Allow  *bool
	Errors []Diagnostic
}

func (r *Evaluation) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Ok)
	FfiDestroyerOptionalBool{}.Destroy(r.Allow)
	FfiDestroyerSequenceDiagnostic{}.Destroy(r.Errors)
}

type FfiConverterEvaluation struct{}

var FfiConverterEvaluationINSTANCE = FfiConverterEvaluation{}

func (c FfiConverterEvaluation) Lift(rb RustBufferI) Evaluation {
	return LiftFromRustBuffer[Evaluation](c, rb)
}

func (c FfiConverterEvaluation) Read(reader io.Reader) Evaluation {
	return Evaluation{
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterSequenceDiagnosticINSTANCE.Read(reader),
	}
}

func (c FfiConverterEvaluation) Lower(value Evaluation) C.RustBuffer {
	return LowerIntoRustBuffer[Evaluation](c, value)
}

func (c FfiConverterEvaluation) LowerExternal(value Evaluation) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Evaluation](c, value))
}

func (c FfiConverterEvaluation) Write(writer io.Writer, value Evaluation) {
	FfiConverterBoolINSTANCE.Write(writer, value.Ok)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.Allow)
	FfiConverterSequenceDiagnosticINSTANCE.Write(writer, value.Errors)
}

type FfiDestroyerEvaluation struct{}

func (_ FfiDestroyerEvaluation) Destroy(value Evaluation) {
	value.Destroy()
}

// How many policy-engine replicas have the policy.
type Loaded struct {
	Replicas uint32
	Total    uint32
}

func (r *Loaded) Destroy() {
	FfiDestroyerUint32{}.Destroy(r.Replicas)
	FfiDestroyerUint32{}.Destroy(r.Total)
}

type FfiConverterLoaded struct{}

var FfiConverterLoadedINSTANCE = FfiConverterLoaded{}

func (c FfiConverterLoaded) Lift(rb RustBufferI) Loaded {
	return LiftFromRustBuffer[Loaded](c, rb)
}

func (c FfiConverterLoaded) Read(reader io.Reader) Loaded {
	return Loaded{
		FfiConverterUint32INSTANCE.Read(reader),
		FfiConverterUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterLoaded) Lower(value Loaded) C.RustBuffer {
	return LowerIntoRustBuffer[Loaded](c, value)
}

func (c FfiConverterLoaded) LowerExternal(value Loaded) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Loaded](c, value))
}

func (c FfiConverterLoaded) Write(writer io.Writer, value Loaded) {
	FfiConverterUint32INSTANCE.Write(writer, value.Replicas)
	FfiConverterUint32INSTANCE.Write(writer, value.Total)
}

type FfiDestroyerLoaded struct{}

func (_ FfiDestroyerLoaded) Destroy(value Loaded) {
	value.Destroy()
}

// Who manages a policy, and where its source lives.
type Management struct {
	Mode ManagementMode
	// Required, and https, when `mode` is `Iac`: where the policy's source
	// is kept. The app links to it.
	ManagedUrl *string
}

func (r *Management) Destroy() {
	FfiDestroyerManagementMode{}.Destroy(r.Mode)
	FfiDestroyerOptionalString{}.Destroy(r.ManagedUrl)
}

type FfiConverterManagement struct{}

var FfiConverterManagementINSTANCE = FfiConverterManagement{}

func (c FfiConverterManagement) Lift(rb RustBufferI) Management {
	return LiftFromRustBuffer[Management](c, rb)
}

func (c FfiConverterManagement) Read(reader io.Reader) Management {
	return Management{
		FfiConverterManagementModeINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterManagement) Lower(value Management) C.RustBuffer {
	return LowerIntoRustBuffer[Management](c, value)
}

func (c FfiConverterManagement) LowerExternal(value Management) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Management](c, value))
}

func (c FfiConverterManagement) Write(writer io.Writer, value Management) {
	FfiConverterManagementModeINSTANCE.Write(writer, value.Mode)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.ManagedUrl)
}

type FfiDestroyerManagement struct{}

func (_ FfiDestroyerManagement) Destroy(value Management) {
	value.Destroy()
}

// Who a token acts as: `GET /v1/me`.
type Me struct {
	Email string
	Name  string
	// Always false with a token: a token is never an admin.
	Admin bool
}

func (r *Me) Destroy() {
	FfiDestroyerString{}.Destroy(r.Email)
	FfiDestroyerString{}.Destroy(r.Name)
	FfiDestroyerBool{}.Destroy(r.Admin)
}

type FfiConverterMe struct{}

var FfiConverterMeINSTANCE = FfiConverterMe{}

func (c FfiConverterMe) Lift(rb RustBufferI) Me {
	return LiftFromRustBuffer[Me](c, rb)
}

func (c FfiConverterMe) Read(reader io.Reader) Me {
	return Me{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterBoolINSTANCE.Read(reader),
	}
}

func (c FfiConverterMe) Lower(value Me) C.RustBuffer {
	return LowerIntoRustBuffer[Me](c, value)
}

func (c FfiConverterMe) LowerExternal(value Me) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Me](c, value))
}

func (c FfiConverterMe) Write(writer io.Writer, value Me) {
	FfiConverterStringINSTANCE.Write(writer, value.Email)
	FfiConverterStringINSTANCE.Write(writer, value.Name)
	FfiConverterBoolINSTANCE.Write(writer, value.Admin)
}

type FfiDestroyerMe struct{}

func (_ FfiDestroyerMe) Destroy(value Me) {
	value.Destroy()
}

// A session's policy, with its source.
type Policy struct {
	Kind       *string
	Version    *int64
	Hash       *string
	State      PolicyState
	Management *Management
	// What was written.
	Source *string
	// The module in force: the last source that compiled.
	Rego     *string
	Errors   []Diagnostic
	Warnings []Diagnostic
	Loaded   *Loaded
	// RFC 3339.
	Updated *string
	// `ui` or `token:<token name>`.
	UpdatedBy *string
}

func (r *Policy) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.Kind)
	FfiDestroyerOptionalInt64{}.Destroy(r.Version)
	FfiDestroyerOptionalString{}.Destroy(r.Hash)
	FfiDestroyerPolicyState{}.Destroy(r.State)
	FfiDestroyerOptionalManagement{}.Destroy(r.Management)
	FfiDestroyerOptionalString{}.Destroy(r.Source)
	FfiDestroyerOptionalString{}.Destroy(r.Rego)
	FfiDestroyerSequenceDiagnostic{}.Destroy(r.Errors)
	FfiDestroyerSequenceDiagnostic{}.Destroy(r.Warnings)
	FfiDestroyerOptionalLoaded{}.Destroy(r.Loaded)
	FfiDestroyerOptionalString{}.Destroy(r.Updated)
	FfiDestroyerOptionalString{}.Destroy(r.UpdatedBy)
}

type FfiConverterPolicy struct{}

var FfiConverterPolicyINSTANCE = FfiConverterPolicy{}

func (c FfiConverterPolicy) Lift(rb RustBufferI) Policy {
	return LiftFromRustBuffer[Policy](c, rb)
}

func (c FfiConverterPolicy) Read(reader io.Reader) Policy {
	return Policy{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalInt64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterPolicyStateINSTANCE.Read(reader),
		FfiConverterOptionalManagementINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterSequenceDiagnosticINSTANCE.Read(reader),
		FfiConverterSequenceDiagnosticINSTANCE.Read(reader),
		FfiConverterOptionalLoadedINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterPolicy) Lower(value Policy) C.RustBuffer {
	return LowerIntoRustBuffer[Policy](c, value)
}

func (c FfiConverterPolicy) LowerExternal(value Policy) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Policy](c, value))
}

func (c FfiConverterPolicy) Write(writer io.Writer, value Policy) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Kind)
	FfiConverterOptionalInt64INSTANCE.Write(writer, value.Version)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Hash)
	FfiConverterPolicyStateINSTANCE.Write(writer, value.State)
	FfiConverterOptionalManagementINSTANCE.Write(writer, value.Management)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Source)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Rego)
	FfiConverterSequenceDiagnosticINSTANCE.Write(writer, value.Errors)
	FfiConverterSequenceDiagnosticINSTANCE.Write(writer, value.Warnings)
	FfiConverterOptionalLoadedINSTANCE.Write(writer, value.Loaded)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Updated)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.UpdatedBy)
}

type FfiDestroyerPolicy struct{}

func (_ FfiDestroyerPolicy) Destroy(value Policy) {
	value.Destroy()
}

// A policy to write: Rego source, and optionally who manages it.
type PolicyInput struct {
	// The Rego module.
	Source string
	// A token may write a policy in `editor` mode only by moving it to
	// `iac` here.
	Management *Management
	// Write only if the policy is still at this version (`If-Match`).
	IfMatchVersion *int64
}

func (r *PolicyInput) Destroy() {
	FfiDestroyerString{}.Destroy(r.Source)
	FfiDestroyerOptionalManagement{}.Destroy(r.Management)
	FfiDestroyerOptionalInt64{}.Destroy(r.IfMatchVersion)
}

type FfiConverterPolicyInput struct{}

var FfiConverterPolicyInputINSTANCE = FfiConverterPolicyInput{}

func (c FfiConverterPolicyInput) Lift(rb RustBufferI) PolicyInput {
	return LiftFromRustBuffer[PolicyInput](c, rb)
}

func (c FfiConverterPolicyInput) Read(reader io.Reader) PolicyInput {
	return PolicyInput{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalManagementINSTANCE.Read(reader),
		FfiConverterOptionalInt64INSTANCE.Read(reader),
	}
}

func (c FfiConverterPolicyInput) Lower(value PolicyInput) C.RustBuffer {
	return LowerIntoRustBuffer[PolicyInput](c, value)
}

func (c FfiConverterPolicyInput) LowerExternal(value PolicyInput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[PolicyInput](c, value))
}

func (c FfiConverterPolicyInput) Write(writer io.Writer, value PolicyInput) {
	FfiConverterStringINSTANCE.Write(writer, value.Source)
	FfiConverterOptionalManagementINSTANCE.Write(writer, value.Management)
	FfiConverterOptionalInt64INSTANCE.Write(writer, value.IfMatchVersion)
}

type FfiDestroyerPolicyInput struct{}

func (_ FfiDestroyerPolicyInput) Destroy(value PolicyInput) {
	value.Destroy()
}

// A ready-made policy.
type PolicyPreset struct {
	// For example `unrestricted` or `no-scripting`.
	Id          string
	Title       string
	Description string
	Kind        string
	Source      string
}

func (r *PolicyPreset) Destroy() {
	FfiDestroyerString{}.Destroy(r.Id)
	FfiDestroyerString{}.Destroy(r.Title)
	FfiDestroyerString{}.Destroy(r.Description)
	FfiDestroyerString{}.Destroy(r.Kind)
	FfiDestroyerString{}.Destroy(r.Source)
}

type FfiConverterPolicyPreset struct{}

var FfiConverterPolicyPresetINSTANCE = FfiConverterPolicyPreset{}

func (c FfiConverterPolicyPreset) Lift(rb RustBufferI) PolicyPreset {
	return LiftFromRustBuffer[PolicyPreset](c, rb)
}

func (c FfiConverterPolicyPreset) Read(reader io.Reader) PolicyPreset {
	return PolicyPreset{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterPolicyPreset) Lower(value PolicyPreset) C.RustBuffer {
	return LowerIntoRustBuffer[PolicyPreset](c, value)
}

func (c FfiConverterPolicyPreset) LowerExternal(value PolicyPreset) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[PolicyPreset](c, value))
}

func (c FfiConverterPolicyPreset) Write(writer io.Writer, value PolicyPreset) {
	FfiConverterStringINSTANCE.Write(writer, value.Id)
	FfiConverterStringINSTANCE.Write(writer, value.Title)
	FfiConverterStringINSTANCE.Write(writer, value.Description)
	FfiConverterStringINSTANCE.Write(writer, value.Kind)
	FfiConverterStringINSTANCE.Write(writer, value.Source)
}

type FfiDestroyerPolicyPreset struct{}

func (_ FfiDestroyerPolicyPreset) Destroy(value PolicyPreset) {
	value.Destroy()
}

// What a session carries about its policy.
type PolicySummary struct {
	// `rego`.
	Kind    *string
	Version *int64
	// Of the policy in force.
	Hash       *string
	State      PolicyState
	Management *Management
}

func (r *PolicySummary) Destroy() {
	FfiDestroyerOptionalString{}.Destroy(r.Kind)
	FfiDestroyerOptionalInt64{}.Destroy(r.Version)
	FfiDestroyerOptionalString{}.Destroy(r.Hash)
	FfiDestroyerPolicyState{}.Destroy(r.State)
	FfiDestroyerOptionalManagement{}.Destroy(r.Management)
}

type FfiConverterPolicySummary struct{}

var FfiConverterPolicySummaryINSTANCE = FfiConverterPolicySummary{}

func (c FfiConverterPolicySummary) Lift(rb RustBufferI) PolicySummary {
	return LiftFromRustBuffer[PolicySummary](c, rb)
}

func (c FfiConverterPolicySummary) Read(reader io.Reader) PolicySummary {
	return PolicySummary{
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalInt64INSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterPolicyStateINSTANCE.Read(reader),
		FfiConverterOptionalManagementINSTANCE.Read(reader),
	}
}

func (c FfiConverterPolicySummary) Lower(value PolicySummary) C.RustBuffer {
	return LowerIntoRustBuffer[PolicySummary](c, value)
}

func (c FfiConverterPolicySummary) LowerExternal(value PolicySummary) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[PolicySummary](c, value))
}

func (c FfiConverterPolicySummary) Write(writer io.Writer, value PolicySummary) {
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Kind)
	FfiConverterOptionalInt64INSTANCE.Write(writer, value.Version)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Hash)
	FfiConverterPolicyStateINSTANCE.Write(writer, value.State)
	FfiConverterOptionalManagementINSTANCE.Write(writer, value.Management)
}

type FfiDestroyerPolicySummary struct{}

func (_ FfiDestroyerPolicySummary) Destroy(value PolicySummary) {
	value.Destroy()
}

// A `run_js` call.
type RunJsRequest struct {
	// JavaScript or TypeScript. Top-level `await` works.
	Code string
	// Heap limit in MB. Minimum 4, default 8.
	HeapMemoryMaxMb *uint32
	// Time limit in seconds, 1 to 300. Default 30.
	ExecutionTimeoutSecs *uint32
}

func (r *RunJsRequest) Destroy() {
	FfiDestroyerString{}.Destroy(r.Code)
	FfiDestroyerOptionalUint32{}.Destroy(r.HeapMemoryMaxMb)
	FfiDestroyerOptionalUint32{}.Destroy(r.ExecutionTimeoutSecs)
}

type FfiConverterRunJsRequest struct{}

var FfiConverterRunJsRequestINSTANCE = FfiConverterRunJsRequest{}

func (c FfiConverterRunJsRequest) Lift(rb RustBufferI) RunJsRequest {
	return LiftFromRustBuffer[RunJsRequest](c, rb)
}

func (c FfiConverterRunJsRequest) Read(reader io.Reader) RunJsRequest {
	return RunJsRequest{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
		FfiConverterOptionalUint32INSTANCE.Read(reader),
	}
}

func (c FfiConverterRunJsRequest) Lower(value RunJsRequest) C.RustBuffer {
	return LowerIntoRustBuffer[RunJsRequest](c, value)
}

func (c FfiConverterRunJsRequest) LowerExternal(value RunJsRequest) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RunJsRequest](c, value))
}

func (c FfiConverterRunJsRequest) Write(writer io.Writer, value RunJsRequest) {
	FfiConverterStringINSTANCE.Write(writer, value.Code)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.HeapMemoryMaxMb)
	FfiConverterOptionalUint32INSTANCE.Write(writer, value.ExecutionTimeoutSecs)
}

type FfiDestroyerRunJsRequest struct{}

func (_ FfiDestroyerRunJsRequest) Destroy(value RunJsRequest) {
	value.Destroy()
}

// What `run_js` answered.
type RunJsResult struct {
	// What the code wrote with `console.log` and its siblings.
	Output string
	// Set when the code threw or ran out of time. The call itself
	// succeeded: this is the program's failure, not the SDK's.
	Error *string
	// What the code attached with `artifact(key, mime, bytes)`.
	Artifacts []ContentBlock
	// The tool's whole JSON answer.
	RawJson string
}

func (r *RunJsResult) Destroy() {
	FfiDestroyerString{}.Destroy(r.Output)
	FfiDestroyerOptionalString{}.Destroy(r.Error)
	FfiDestroyerSequenceContentBlock{}.Destroy(r.Artifacts)
	FfiDestroyerString{}.Destroy(r.RawJson)
}

type FfiConverterRunJsResult struct{}

var FfiConverterRunJsResultINSTANCE = FfiConverterRunJsResult{}

func (c FfiConverterRunJsResult) Lift(rb RustBufferI) RunJsResult {
	return LiftFromRustBuffer[RunJsResult](c, rb)
}

func (c FfiConverterRunJsResult) Read(reader io.Reader) RunJsResult {
	return RunJsResult{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterSequenceContentBlockINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterRunJsResult) Lower(value RunJsResult) C.RustBuffer {
	return LowerIntoRustBuffer[RunJsResult](c, value)
}

func (c FfiConverterRunJsResult) LowerExternal(value RunJsResult) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[RunJsResult](c, value))
}

func (c FfiConverterRunJsResult) Write(writer io.Writer, value RunJsResult) {
	FfiConverterStringINSTANCE.Write(writer, value.Output)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Error)
	FfiConverterSequenceContentBlockINSTANCE.Write(writer, value.Artifacts)
	FfiConverterStringINSTANCE.Write(writer, value.RawJson)
}

type FfiDestroyerRunJsResult struct{}

func (_ FfiDestroyerRunJsResult) Destroy(value RunJsResult) {
	value.Destroy()
}

// A session as the API shows it.
type SessionInfo struct {
	// `s-` and five or ten characters.
	Id    string
	Name  string
	Owner string
	State SessionState
	// Why it is starting or failed.
	Message *string
	// RFC 3339.
	Created *string
	// The MCP URL the API reports, which is the one people sign in to. A
	// token uses [`Session::mcp_url`](crate::Session::mcp_url) instead.
	McpUrl *string
	// Absent where policies are off.
	Policy *PolicySummary
	// True when a suspended session holds a snapshot of its running
	// desktop, which a wake restores. Absent otherwise.
	StateSaved *bool
	// Why an asleep or stopped session is so (`user`, `sleep`, `idle`,
	// `credit`, `payment-method`, `blocked`). Only where billing is on.
	StoppedBy *string
	// The same reasons, while it finishes its calls before such a sleep.
	Draining *string
	// RFC 3339: when it will be deleted for its account being at zero.
	DeleteAfter *string
}

func (r *SessionInfo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Id)
	FfiDestroyerString{}.Destroy(r.Name)
	FfiDestroyerString{}.Destroy(r.Owner)
	FfiDestroyerSessionState{}.Destroy(r.State)
	FfiDestroyerOptionalString{}.Destroy(r.Message)
	FfiDestroyerOptionalString{}.Destroy(r.Created)
	FfiDestroyerOptionalString{}.Destroy(r.McpUrl)
	FfiDestroyerOptionalPolicySummary{}.Destroy(r.Policy)
	FfiDestroyerOptionalBool{}.Destroy(r.StateSaved)
	FfiDestroyerOptionalString{}.Destroy(r.StoppedBy)
	FfiDestroyerOptionalString{}.Destroy(r.Draining)
	FfiDestroyerOptionalString{}.Destroy(r.DeleteAfter)
}

type FfiConverterSessionInfo struct{}

var FfiConverterSessionInfoINSTANCE = FfiConverterSessionInfo{}

func (c FfiConverterSessionInfo) Lift(rb RustBufferI) SessionInfo {
	return LiftFromRustBuffer[SessionInfo](c, rb)
}

func (c FfiConverterSessionInfo) Read(reader io.Reader) SessionInfo {
	return SessionInfo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterSessionStateINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalPolicySummaryINSTANCE.Read(reader),
		FfiConverterOptionalBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterSessionInfo) Lower(value SessionInfo) C.RustBuffer {
	return LowerIntoRustBuffer[SessionInfo](c, value)
}

func (c FfiConverterSessionInfo) LowerExternal(value SessionInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SessionInfo](c, value))
}

func (c FfiConverterSessionInfo) Write(writer io.Writer, value SessionInfo) {
	FfiConverterStringINSTANCE.Write(writer, value.Id)
	FfiConverterStringINSTANCE.Write(writer, value.Name)
	FfiConverterStringINSTANCE.Write(writer, value.Owner)
	FfiConverterSessionStateINSTANCE.Write(writer, value.State)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Message)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Created)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.McpUrl)
	FfiConverterOptionalPolicySummaryINSTANCE.Write(writer, value.Policy)
	FfiConverterOptionalBoolINSTANCE.Write(writer, value.StateSaved)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.StoppedBy)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Draining)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.DeleteAfter)
}

type FfiDestroyerSessionInfo struct{}

func (_ FfiDestroyerSessionInfo) Destroy(value SessionInfo) {
	value.Destroy()
}

// A tool of the session's MCP server.
type ToolInfo struct {
	Name        string
	Description *string
	// The JSON Schema of its arguments.
	InputSchemaJson string
}

func (r *ToolInfo) Destroy() {
	FfiDestroyerString{}.Destroy(r.Name)
	FfiDestroyerOptionalString{}.Destroy(r.Description)
	FfiDestroyerString{}.Destroy(r.InputSchemaJson)
}

type FfiConverterToolInfo struct{}

var FfiConverterToolInfoINSTANCE = FfiConverterToolInfo{}

func (c FfiConverterToolInfo) Lift(rb RustBufferI) ToolInfo {
	return LiftFromRustBuffer[ToolInfo](c, rb)
}

func (c FfiConverterToolInfo) Read(reader io.Reader) ToolInfo {
	return ToolInfo{
		FfiConverterStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterToolInfo) Lower(value ToolInfo) C.RustBuffer {
	return LowerIntoRustBuffer[ToolInfo](c, value)
}

func (c FfiConverterToolInfo) LowerExternal(value ToolInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ToolInfo](c, value))
}

func (c FfiConverterToolInfo) Write(writer io.Writer, value ToolInfo) {
	FfiConverterStringINSTANCE.Write(writer, value.Name)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Description)
	FfiConverterStringINSTANCE.Write(writer, value.InputSchemaJson)
}

type FfiDestroyerToolInfo struct{}

func (_ FfiDestroyerToolInfo) Destroy(value ToolInfo) {
	value.Destroy()
}

// What a tool answered.
type ToolResult struct {
	Content []ContentBlock
	// `structuredContent`, as JSON, when the tool sent one.
	StructuredJson *string
}

func (r *ToolResult) Destroy() {
	FfiDestroyerSequenceContentBlock{}.Destroy(r.Content)
	FfiDestroyerOptionalString{}.Destroy(r.StructuredJson)
}

type FfiConverterToolResult struct{}

var FfiConverterToolResultINSTANCE = FfiConverterToolResult{}

func (c FfiConverterToolResult) Lift(rb RustBufferI) ToolResult {
	return LiftFromRustBuffer[ToolResult](c, rb)
}

func (c FfiConverterToolResult) Read(reader io.Reader) ToolResult {
	return ToolResult{
		FfiConverterSequenceContentBlockINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
	}
}

func (c FfiConverterToolResult) Lower(value ToolResult) C.RustBuffer {
	return LowerIntoRustBuffer[ToolResult](c, value)
}

func (c FfiConverterToolResult) LowerExternal(value ToolResult) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ToolResult](c, value))
}

func (c FfiConverterToolResult) Write(writer io.Writer, value ToolResult) {
	FfiConverterSequenceContentBlockINSTANCE.Write(writer, value.Content)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.StructuredJson)
}

type FfiDestroyerToolResult struct{}

func (_ FfiDestroyerToolResult) Destroy(value ToolResult) {
	value.Destroy()
}

// The verdict on a policy that was checked and not saved.
type Validation struct {
	Ok       bool
	Rego     *string
	Hash     *string
	Errors   []Diagnostic
	Warnings []Diagnostic
}

func (r *Validation) Destroy() {
	FfiDestroyerBool{}.Destroy(r.Ok)
	FfiDestroyerOptionalString{}.Destroy(r.Rego)
	FfiDestroyerOptionalString{}.Destroy(r.Hash)
	FfiDestroyerSequenceDiagnostic{}.Destroy(r.Errors)
	FfiDestroyerSequenceDiagnostic{}.Destroy(r.Warnings)
}

type FfiConverterValidation struct{}

var FfiConverterValidationINSTANCE = FfiConverterValidation{}

func (c FfiConverterValidation) Lift(rb RustBufferI) Validation {
	return LiftFromRustBuffer[Validation](c, rb)
}

func (c FfiConverterValidation) Read(reader io.Reader) Validation {
	return Validation{
		FfiConverterBoolINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterOptionalStringINSTANCE.Read(reader),
		FfiConverterSequenceDiagnosticINSTANCE.Read(reader),
		FfiConverterSequenceDiagnosticINSTANCE.Read(reader),
	}
}

func (c FfiConverterValidation) Lower(value Validation) C.RustBuffer {
	return LowerIntoRustBuffer[Validation](c, value)
}

func (c FfiConverterValidation) LowerExternal(value Validation) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[Validation](c, value))
}

func (c FfiConverterValidation) Write(writer io.Writer, value Validation) {
	FfiConverterBoolINSTANCE.Write(writer, value.Ok)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Rego)
	FfiConverterOptionalStringINSTANCE.Write(writer, value.Hash)
	FfiConverterSequenceDiagnosticINSTANCE.Write(writer, value.Errors)
	FfiConverterSequenceDiagnosticINSTANCE.Write(writer, value.Warnings)
}

type FfiDestroyerValidation struct{}

func (_ FfiDestroyerValidation) Destroy(value Validation) {
	value.Destroy()
}

// A builder was asked to build without a required field.
type BuildError struct {
	err error
}

// Convenience method to turn *BuildError into error
// Avoiding treating nil pointer as non nil error interface
func (err *BuildError) AsError() error {
	if err == nil {
		return nil
	} else {
		return err
	}
}

func (err BuildError) Error() string {
	return fmt.Sprintf("BuildError: %s", err.err.Error())
}

func (err BuildError) Unwrap() error {
	return err.err
}

// Err* are used for checking error type with `errors.Is`
var ErrBuildErrorMissingRequiredField = fmt.Errorf("BuildErrorMissingRequiredField")

// Variant structs
type BuildErrorMissingRequiredField struct {
	RecordType string
	Field      string
}

func NewBuildErrorMissingRequiredField(
	recordType string,
	field string,
) *BuildError {
	return &BuildError{err: &BuildErrorMissingRequiredField{
		RecordType: recordType,
		Field:      field}}
}

func (e BuildErrorMissingRequiredField) destroy() {
	FfiDestroyerString{}.Destroy(e.RecordType)
	FfiDestroyerString{}.Destroy(e.Field)
}

func (err BuildErrorMissingRequiredField) Error() string {
	return fmt.Sprint("MissingRequiredField",
		": ",

		"RecordType=",
		err.RecordType,
		", ",
		"Field=",
		err.Field,
	)
}

func (self BuildErrorMissingRequiredField) Is(target error) bool {
	return target == ErrBuildErrorMissingRequiredField
}

type FfiConverterBuildError struct{}

var FfiConverterBuildErrorINSTANCE = FfiConverterBuildError{}

func (c FfiConverterBuildError) Lift(eb RustBufferI) *BuildError {
	return LiftFromRustBuffer[*BuildError](c, eb)
}

func (c FfiConverterBuildError) Lower(value *BuildError) C.RustBuffer {
	return LowerIntoRustBuffer[*BuildError](c, value)
}

func (c FfiConverterBuildError) LowerExternal(value *BuildError) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*BuildError](c, value))
}

func (c FfiConverterBuildError) Read(reader io.Reader) *BuildError {
	errorID := readUint32(reader)

	switch errorID {
	case 1:
		return &BuildError{&BuildErrorMissingRequiredField{
			RecordType: FfiConverterStringINSTANCE.Read(reader),
			Field:      FfiConverterStringINSTANCE.Read(reader),
		}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterBuildError.Read()", errorID))
	}
}

func (c FfiConverterBuildError) Write(writer io.Writer, value *BuildError) {
	switch variantValue := value.err.(type) {
	case *BuildErrorMissingRequiredField:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variantValue.RecordType)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Field)
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiConverterBuildError.Write", value))
	}
}

type FfiDestroyerBuildError struct{}

func (_ FfiDestroyerBuildError) Destroy(value *BuildError) {
	switch variantValue := value.err.(type) {
	case BuildErrorMissingRequiredField:
		variantValue.destroy()
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiDestroyerBuildError.Destroy", value))
	}
}

// Everything a call can fail with.
//
// The HTTP answers the API documents each have a variant; any other status
// is [`ComputerUseError::Api`]. No variant carries a token.
type ComputerUseError struct {
	err error
}

// Convenience method to turn *ComputerUseError into error
// Avoiding treating nil pointer as non nil error interface
func (err *ComputerUseError) AsError() error {
	if err == nil {
		return nil
	} else {
		return err
	}
}

func (err ComputerUseError) Error() string {
	return fmt.Sprintf("ComputerUseError: %s", err.err.Error())
}

func (err ComputerUseError) Unwrap() error {
	return err.err
}

// Err* are used for checking error type with `errors.Is`
var ErrComputerUseErrorConfiguration = fmt.Errorf("ComputerUseErrorConfiguration")
var ErrComputerUseErrorTransport = fmt.Errorf("ComputerUseErrorTransport")
var ErrComputerUseErrorTimeout = fmt.Errorf("ComputerUseErrorTimeout")
var ErrComputerUseErrorUnauthorized = fmt.Errorf("ComputerUseErrorUnauthorized")
var ErrComputerUseErrorPaymentRequired = fmt.Errorf("ComputerUseErrorPaymentRequired")
var ErrComputerUseErrorForbidden = fmt.Errorf("ComputerUseErrorForbidden")
var ErrComputerUseErrorNotFound = fmt.Errorf("ComputerUseErrorNotFound")
var ErrComputerUseErrorConflict = fmt.Errorf("ComputerUseErrorConflict")
var ErrComputerUseErrorInvalidPolicy = fmt.Errorf("ComputerUseErrorInvalidPolicy")
var ErrComputerUseErrorRateLimited = fmt.Errorf("ComputerUseErrorRateLimited")
var ErrComputerUseErrorApi = fmt.Errorf("ComputerUseErrorApi")
var ErrComputerUseErrorDecode = fmt.Errorf("ComputerUseErrorDecode")
var ErrComputerUseErrorMcp = fmt.Errorf("ComputerUseErrorMcp")
var ErrComputerUseErrorTool = fmt.Errorf("ComputerUseErrorTool")
var ErrComputerUseErrorSessionFailed = fmt.Errorf("ComputerUseErrorSessionFailed")

// Variant structs
// The client was configured with something it cannot use.
type ComputerUseErrorConfiguration struct {
	Reason string
}

// The client was configured with something it cannot use.
func NewComputerUseErrorConfiguration(
	reason string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorConfiguration{
		Reason: reason}}
}

func (e ComputerUseErrorConfiguration) destroy() {
	FfiDestroyerString{}.Destroy(e.Reason)
}

func (err ComputerUseErrorConfiguration) Error() string {
	return fmt.Sprint("Configuration",
		": ",

		"Reason=",
		err.Reason,
	)
}

func (self ComputerUseErrorConfiguration) Is(target error) bool {
	return target == ErrComputerUseErrorConfiguration
}

// The request did not get an answer: DNS, TLS, a refused or dropped
// connection.
type ComputerUseErrorTransport struct {
	Reason string
}

// The request did not get an answer: DNS, TLS, a refused or dropped
// connection.
func NewComputerUseErrorTransport(
	reason string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorTransport{
		Reason: reason}}
}

func (e ComputerUseErrorTransport) destroy() {
	FfiDestroyerString{}.Destroy(e.Reason)
}

func (err ComputerUseErrorTransport) Error() string {
	return fmt.Sprint("Transport",
		": ",

		"Reason=",
		err.Reason,
	)
}

func (self ComputerUseErrorTransport) Is(target error) bool {
	return target == ErrComputerUseErrorTransport
}

// No answer within the client's timeout, or a wait that ran out.
type ComputerUseErrorTimeout struct {
	Operation string
}

// No answer within the client's timeout, or a wait that ran out.
func NewComputerUseErrorTimeout(
	operation string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorTimeout{
		Operation: operation}}
}

func (e ComputerUseErrorTimeout) destroy() {
	FfiDestroyerString{}.Destroy(e.Operation)
}

func (err ComputerUseErrorTimeout) Error() string {
	return fmt.Sprint("Timeout",
		": ",

		"Operation=",
		err.Operation,
	)
}

func (self ComputerUseErrorTimeout) Is(target error) bool {
	return target == ErrComputerUseErrorTimeout
}

// 401: the token is missing, malformed, unknown, revoked or expired, or
// its owner may no longer use the service.
type ComputerUseErrorUnauthorized struct {
	Message string
}

// 401: the token is missing, malformed, unknown, revoked or expired, or
// its owner may no longer use the service.
func NewComputerUseErrorUnauthorized(
	message string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorUnauthorized{
		Message: message}}
}

func (e ComputerUseErrorUnauthorized) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err ComputerUseErrorUnauthorized) Error() string {
	return fmt.Sprint("Unauthorized",
		": ",

		"Message=",
		err.Message,
	)
}

func (self ComputerUseErrorUnauthorized) Is(target error) bool {
	return target == ErrComputerUseErrorUnauthorized
}

// 402: billing refused the request. `code` says what for and
// `billing_url` is where the account's owner puts it right.
type ComputerUseErrorPaymentRequired struct {
	Message    string
	Code       *string
	BillingUrl *string
}

// 402: billing refused the request. `code` says what for and
// `billing_url` is where the account's owner puts it right.
func NewComputerUseErrorPaymentRequired(
	message string,
	code *string,
	billingUrl *string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorPaymentRequired{
		Message:    message,
		Code:       code,
		BillingUrl: billingUrl}}
}

func (e ComputerUseErrorPaymentRequired) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
	FfiDestroyerOptionalString{}.Destroy(e.Code)
	FfiDestroyerOptionalString{}.Destroy(e.BillingUrl)
}

func (err ComputerUseErrorPaymentRequired) Error() string {
	return fmt.Sprint("PaymentRequired",
		": ",

		"Message=",
		err.Message,
		", ",
		"Code=",
		err.Code,
		", ",
		"BillingUrl=",
		err.BillingUrl,
	)
}

func (self ComputerUseErrorPaymentRequired) Is(target error) bool {
	return target == ErrComputerUseErrorPaymentRequired
}

// 403: the token lacks the scope the route needs, or is bound to
// another session.
type ComputerUseErrorForbidden struct {
	Message string
}

// 403: the token lacks the scope the route needs, or is bound to
// another session.
func NewComputerUseErrorForbidden(
	message string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorForbidden{
		Message: message}}
}

func (e ComputerUseErrorForbidden) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err ComputerUseErrorForbidden) Error() string {
	return fmt.Sprint("Forbidden",
		": ",

		"Message=",
		err.Message,
	)
}

func (self ComputerUseErrorForbidden) Is(target error) bool {
	return target == ErrComputerUseErrorForbidden
}

// 404: no such session, or not the caller's. The API answers both
// alike.
type ComputerUseErrorNotFound struct {
	Message string
}

// 404: no such session, or not the caller's. The API answers both
// alike.
func NewComputerUseErrorNotFound(
	message string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorNotFound{
		Message: message}}
}

func (e ComputerUseErrorNotFound) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err ComputerUseErrorNotFound) Error() string {
	return fmt.Sprint("NotFound",
		": ",

		"Message=",
		err.Message,
	)
}

func (self ComputerUseErrorNotFound) Is(target error) bool {
	return target == ErrComputerUseErrorNotFound
}

// 409: the session limit is reached, the session is stopped, or the
// policy is managed elsewhere (`managed_url` then says where).
type ComputerUseErrorConflict struct {
	Message    string
	Code       *string
	ManagedUrl *string
}

// 409: the session limit is reached, the session is stopped, or the
// policy is managed elsewhere (`managed_url` then says where).
func NewComputerUseErrorConflict(
	message string,
	code *string,
	managedUrl *string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorConflict{
		Message:    message,
		Code:       code,
		ManagedUrl: managedUrl}}
}

func (e ComputerUseErrorConflict) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
	FfiDestroyerOptionalString{}.Destroy(e.Code)
	FfiDestroyerOptionalString{}.Destroy(e.ManagedUrl)
}

func (err ComputerUseErrorConflict) Error() string {
	return fmt.Sprint("Conflict",
		": ",

		"Message=",
		err.Message,
		", ",
		"Code=",
		err.Code,
		", ",
		"ManagedUrl=",
		err.ManagedUrl,
	)
}

func (self ComputerUseErrorConflict) Is(target error) bool {
	return target == ErrComputerUseErrorConflict
}

// 422: the policy does not validate. Nothing was saved.
type ComputerUseErrorInvalidPolicy struct {
	Message  string
	Errors   []Diagnostic
	Warnings []Diagnostic
}

// 422: the policy does not validate. Nothing was saved.
func NewComputerUseErrorInvalidPolicy(
	message string,
	errors []Diagnostic,
	warnings []Diagnostic,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorInvalidPolicy{
		Message:  message,
		Errors:   errors,
		Warnings: warnings}}
}

func (e ComputerUseErrorInvalidPolicy) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
	FfiDestroyerSequenceDiagnostic{}.Destroy(e.Errors)
	FfiDestroyerSequenceDiagnostic{}.Destroy(e.Warnings)
}

func (err ComputerUseErrorInvalidPolicy) Error() string {
	return fmt.Sprint("InvalidPolicy",
		": ",

		"Message=",
		err.Message,
		", ",
		"Errors=",
		err.Errors,
		", ",
		"Warnings=",
		err.Warnings,
	)
}

func (self ComputerUseErrorInvalidPolicy) Is(target error) bool {
	return target == ErrComputerUseErrorInvalidPolicy
}

// 429, after the client's own retries.
type ComputerUseErrorRateLimited struct {
	Message        string
	RetryAfterSecs *uint64
}

// 429, after the client's own retries.
func NewComputerUseErrorRateLimited(
	message string,
	retryAfterSecs *uint64,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorRateLimited{
		Message:        message,
		RetryAfterSecs: retryAfterSecs}}
}

func (e ComputerUseErrorRateLimited) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
	FfiDestroyerOptionalUint64{}.Destroy(e.RetryAfterSecs)
}

func (err ComputerUseErrorRateLimited) Error() string {
	return fmt.Sprint("RateLimited",
		": ",

		"Message=",
		err.Message,
		", ",
		"RetryAfterSecs=",
		err.RetryAfterSecs,
	)
}

func (self ComputerUseErrorRateLimited) Is(target error) bool {
	return target == ErrComputerUseErrorRateLimited
}

// Any other status that is not a success.
type ComputerUseErrorApi struct {
	Status  uint16
	Message string
	Code    *string
}

// Any other status that is not a success.
func NewComputerUseErrorApi(
	status uint16,
	message string,
	code *string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorApi{
		Status:  status,
		Message: message,
		Code:    code}}
}

func (e ComputerUseErrorApi) destroy() {
	FfiDestroyerUint16{}.Destroy(e.Status)
	FfiDestroyerString{}.Destroy(e.Message)
	FfiDestroyerOptionalString{}.Destroy(e.Code)
}

func (err ComputerUseErrorApi) Error() string {
	return fmt.Sprint("Api",
		": ",

		"Status=",
		err.Status,
		", ",
		"Message=",
		err.Message,
		", ",
		"Code=",
		err.Code,
	)
}

func (self ComputerUseErrorApi) Is(target error) bool {
	return target == ErrComputerUseErrorApi
}

// A success whose body was not what the API documents.
type ComputerUseErrorDecode struct {
	Reason string
}

// A success whose body was not what the API documents.
func NewComputerUseErrorDecode(
	reason string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorDecode{
		Reason: reason}}
}

func (e ComputerUseErrorDecode) destroy() {
	FfiDestroyerString{}.Destroy(e.Reason)
}

func (err ComputerUseErrorDecode) Error() string {
	return fmt.Sprint("Decode",
		": ",

		"Reason=",
		err.Reason,
	)
}

func (self ComputerUseErrorDecode) Is(target error) bool {
	return target == ErrComputerUseErrorDecode
}

// The session's MCP server answered a JSON-RPC error.
type ComputerUseErrorMcp struct {
	Code    int64
	Message string
}

// The session's MCP server answered a JSON-RPC error.
func NewComputerUseErrorMcp(
	code int64,
	message string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorMcp{
		Code:    code,
		Message: message}}
}

func (e ComputerUseErrorMcp) destroy() {
	FfiDestroyerInt64{}.Destroy(e.Code)
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err ComputerUseErrorMcp) Error() string {
	return fmt.Sprint("Mcp",
		": ",

		"Code=",
		err.Code,
		", ",
		"Message=",
		err.Message,
	)
}

func (self ComputerUseErrorMcp) Is(target error) bool {
	return target == ErrComputerUseErrorMcp
}

// A tool reported that it failed (`isError`).
type ComputerUseErrorTool struct {
	Tool    string
	Message string
}

// A tool reported that it failed (`isError`).
func NewComputerUseErrorTool(
	tool string,
	message string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorTool{
		Tool:    tool,
		Message: message}}
}

func (e ComputerUseErrorTool) destroy() {
	FfiDestroyerString{}.Destroy(e.Tool)
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err ComputerUseErrorTool) Error() string {
	return fmt.Sprint("Tool",
		": ",

		"Tool=",
		err.Tool,
		", ",
		"Message=",
		err.Message,
	)
}

func (self ComputerUseErrorTool) Is(target error) bool {
	return target == ErrComputerUseErrorTool
}

// The session went to `failed` while it was waited for.
type ComputerUseErrorSessionFailed struct {
	Message string
}

// The session went to `failed` while it was waited for.
func NewComputerUseErrorSessionFailed(
	message string,
) *ComputerUseError {
	return &ComputerUseError{err: &ComputerUseErrorSessionFailed{
		Message: message}}
}

func (e ComputerUseErrorSessionFailed) destroy() {
	FfiDestroyerString{}.Destroy(e.Message)
}

func (err ComputerUseErrorSessionFailed) Error() string {
	return fmt.Sprint("SessionFailed",
		": ",

		"Message=",
		err.Message,
	)
}

func (self ComputerUseErrorSessionFailed) Is(target error) bool {
	return target == ErrComputerUseErrorSessionFailed
}

type FfiConverterComputerUseError struct{}

var FfiConverterComputerUseErrorINSTANCE = FfiConverterComputerUseError{}

func (c FfiConverterComputerUseError) Lift(eb RustBufferI) *ComputerUseError {
	return LiftFromRustBuffer[*ComputerUseError](c, eb)
}

func (c FfiConverterComputerUseError) Lower(value *ComputerUseError) C.RustBuffer {
	return LowerIntoRustBuffer[*ComputerUseError](c, value)
}

func (c FfiConverterComputerUseError) LowerExternal(value *ComputerUseError) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*ComputerUseError](c, value))
}

func (c FfiConverterComputerUseError) Read(reader io.Reader) *ComputerUseError {
	errorID := readUint32(reader)

	switch errorID {
	case 1:
		return &ComputerUseError{&ComputerUseErrorConfiguration{
			Reason: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 2:
		return &ComputerUseError{&ComputerUseErrorTransport{
			Reason: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 3:
		return &ComputerUseError{&ComputerUseErrorTimeout{
			Operation: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 4:
		return &ComputerUseError{&ComputerUseErrorUnauthorized{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 5:
		return &ComputerUseError{&ComputerUseErrorPaymentRequired{
			Message:    FfiConverterStringINSTANCE.Read(reader),
			Code:       FfiConverterOptionalStringINSTANCE.Read(reader),
			BillingUrl: FfiConverterOptionalStringINSTANCE.Read(reader),
		}}
	case 6:
		return &ComputerUseError{&ComputerUseErrorForbidden{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 7:
		return &ComputerUseError{&ComputerUseErrorNotFound{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 8:
		return &ComputerUseError{&ComputerUseErrorConflict{
			Message:    FfiConverterStringINSTANCE.Read(reader),
			Code:       FfiConverterOptionalStringINSTANCE.Read(reader),
			ManagedUrl: FfiConverterOptionalStringINSTANCE.Read(reader),
		}}
	case 9:
		return &ComputerUseError{&ComputerUseErrorInvalidPolicy{
			Message:  FfiConverterStringINSTANCE.Read(reader),
			Errors:   FfiConverterSequenceDiagnosticINSTANCE.Read(reader),
			Warnings: FfiConverterSequenceDiagnosticINSTANCE.Read(reader),
		}}
	case 10:
		return &ComputerUseError{&ComputerUseErrorRateLimited{
			Message:        FfiConverterStringINSTANCE.Read(reader),
			RetryAfterSecs: FfiConverterOptionalUint64INSTANCE.Read(reader),
		}}
	case 11:
		return &ComputerUseError{&ComputerUseErrorApi{
			Status:  FfiConverterUint16INSTANCE.Read(reader),
			Message: FfiConverterStringINSTANCE.Read(reader),
			Code:    FfiConverterOptionalStringINSTANCE.Read(reader),
		}}
	case 12:
		return &ComputerUseError{&ComputerUseErrorDecode{
			Reason: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 13:
		return &ComputerUseError{&ComputerUseErrorMcp{
			Code:    FfiConverterInt64INSTANCE.Read(reader),
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 14:
		return &ComputerUseError{&ComputerUseErrorTool{
			Tool:    FfiConverterStringINSTANCE.Read(reader),
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	case 15:
		return &ComputerUseError{&ComputerUseErrorSessionFailed{
			Message: FfiConverterStringINSTANCE.Read(reader),
		}}
	default:
		panic(fmt.Sprintf("Unknown error code %d in FfiConverterComputerUseError.Read()", errorID))
	}
}

func (c FfiConverterComputerUseError) Write(writer io.Writer, value *ComputerUseError) {
	switch variantValue := value.err.(type) {
	case *ComputerUseErrorConfiguration:
		writeInt32(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Reason)
	case *ComputerUseErrorTransport:
		writeInt32(writer, 2)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Reason)
	case *ComputerUseErrorTimeout:
		writeInt32(writer, 3)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Operation)
	case *ComputerUseErrorUnauthorized:
		writeInt32(writer, 4)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *ComputerUseErrorPaymentRequired:
		writeInt32(writer, 5)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
		FfiConverterOptionalStringINSTANCE.Write(writer, variantValue.Code)
		FfiConverterOptionalStringINSTANCE.Write(writer, variantValue.BillingUrl)
	case *ComputerUseErrorForbidden:
		writeInt32(writer, 6)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *ComputerUseErrorNotFound:
		writeInt32(writer, 7)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *ComputerUseErrorConflict:
		writeInt32(writer, 8)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
		FfiConverterOptionalStringINSTANCE.Write(writer, variantValue.Code)
		FfiConverterOptionalStringINSTANCE.Write(writer, variantValue.ManagedUrl)
	case *ComputerUseErrorInvalidPolicy:
		writeInt32(writer, 9)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
		FfiConverterSequenceDiagnosticINSTANCE.Write(writer, variantValue.Errors)
		FfiConverterSequenceDiagnosticINSTANCE.Write(writer, variantValue.Warnings)
	case *ComputerUseErrorRateLimited:
		writeInt32(writer, 10)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
		FfiConverterOptionalUint64INSTANCE.Write(writer, variantValue.RetryAfterSecs)
	case *ComputerUseErrorApi:
		writeInt32(writer, 11)
		FfiConverterUint16INSTANCE.Write(writer, variantValue.Status)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
		FfiConverterOptionalStringINSTANCE.Write(writer, variantValue.Code)
	case *ComputerUseErrorDecode:
		writeInt32(writer, 12)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Reason)
	case *ComputerUseErrorMcp:
		writeInt32(writer, 13)
		FfiConverterInt64INSTANCE.Write(writer, variantValue.Code)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *ComputerUseErrorTool:
		writeInt32(writer, 14)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Tool)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	case *ComputerUseErrorSessionFailed:
		writeInt32(writer, 15)
		FfiConverterStringINSTANCE.Write(writer, variantValue.Message)
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiConverterComputerUseError.Write", value))
	}
}

type FfiDestroyerComputerUseError struct{}

func (_ FfiDestroyerComputerUseError) Destroy(value *ComputerUseError) {
	switch variantValue := value.err.(type) {
	case ComputerUseErrorConfiguration:
		variantValue.destroy()
	case ComputerUseErrorTransport:
		variantValue.destroy()
	case ComputerUseErrorTimeout:
		variantValue.destroy()
	case ComputerUseErrorUnauthorized:
		variantValue.destroy()
	case ComputerUseErrorPaymentRequired:
		variantValue.destroy()
	case ComputerUseErrorForbidden:
		variantValue.destroy()
	case ComputerUseErrorNotFound:
		variantValue.destroy()
	case ComputerUseErrorConflict:
		variantValue.destroy()
	case ComputerUseErrorInvalidPolicy:
		variantValue.destroy()
	case ComputerUseErrorRateLimited:
		variantValue.destroy()
	case ComputerUseErrorApi:
		variantValue.destroy()
	case ComputerUseErrorDecode:
		variantValue.destroy()
	case ComputerUseErrorMcp:
		variantValue.destroy()
	case ComputerUseErrorTool:
		variantValue.destroy()
	case ComputerUseErrorSessionFailed:
		variantValue.destroy()
	default:
		_ = variantValue
		panic(fmt.Sprintf("invalid error value `%v` in FfiDestroyerComputerUseError.Destroy", value))
	}
}

// Who manages a policy.
type ManagementMode uint

const (
	// The editor in the app. A token may not write it, unless the write
	// moves it to `Iac`.
	ManagementModeEditor ManagementMode = 1
	// Code: Terraform, or this SDK. The app may not write it.
	ManagementModeIac ManagementMode = 2
	// A mode this version of the SDK does not know.
	ManagementModeUnknown ManagementMode = 3
)

type FfiConverterManagementMode struct{}

var FfiConverterManagementModeINSTANCE = FfiConverterManagementMode{}

func (c FfiConverterManagementMode) Lift(rb RustBufferI) ManagementMode {
	return LiftFromRustBuffer[ManagementMode](c, rb)
}

func (c FfiConverterManagementMode) Lower(value ManagementMode) C.RustBuffer {
	return LowerIntoRustBuffer[ManagementMode](c, value)
}

func (c FfiConverterManagementMode) LowerExternal(value ManagementMode) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[ManagementMode](c, value))
}
func (FfiConverterManagementMode) Read(reader io.Reader) ManagementMode {
	id := readInt32(reader)
	return ManagementMode(id)
}

func (FfiConverterManagementMode) Write(writer io.Writer, value ManagementMode) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerManagementMode struct{}

func (_ FfiDestroyerManagementMode) Destroy(value ManagementMode) {
}

// Whether a policy is in force.
type PolicyState uint

const (
	// In force on every replica.
	PolicyStateReady PolicyState = 1
	// Saved; not yet loaded everywhere.
	PolicyStateLoading PolicyState = 2
	// Does not compile; the previous policy, if any, is still in force.
	PolicyStateInvalid PolicyState = 3
	// The session predates policies and cannot have one.
	PolicyStateUnsupported PolicyState = 4
	// A state this version of the SDK does not know.
	PolicyStateUnknown PolicyState = 5
)

type FfiConverterPolicyState struct{}

var FfiConverterPolicyStateINSTANCE = FfiConverterPolicyState{}

func (c FfiConverterPolicyState) Lift(rb RustBufferI) PolicyState {
	return LiftFromRustBuffer[PolicyState](c, rb)
}

func (c FfiConverterPolicyState) Lower(value PolicyState) C.RustBuffer {
	return LowerIntoRustBuffer[PolicyState](c, value)
}

func (c FfiConverterPolicyState) LowerExternal(value PolicyState) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[PolicyState](c, value))
}
func (FfiConverterPolicyState) Read(reader io.Reader) PolicyState {
	id := readInt32(reader)
	return PolicyState(id)
}

func (FfiConverterPolicyState) Write(writer io.Writer, value PolicyState) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerPolicyState struct{}

func (_ FfiDestroyerPolicyState) Destroy(value PolicyState) {
}

// A session's lifecycle state.
type SessionState uint

const (
	// The desktop is starting or waking.
	SessionStateStarting SessionState = 1
	// The desktop is up.
	SessionStateRunning SessionState = 2
	// The desktop is shutting down: a stop, a sleep or a delete.
	SessionStateStopping SessionState = 3
	// No compute; disk and snapshot kept. An MCP call wakes it.
	SessionStateAsleep SessionState = 4
	// Stopped on request; disk kept. An MCP call answers 409.
	SessionStateStopped SessionState = 5
	// The desktop could not start; `message` says why.
	SessionStateFailed SessionState = 6
	// A state this version of the SDK does not know.
	SessionStateUnknown SessionState = 7
)

type FfiConverterSessionState struct{}

var FfiConverterSessionStateINSTANCE = FfiConverterSessionState{}

func (c FfiConverterSessionState) Lift(rb RustBufferI) SessionState {
	return LiftFromRustBuffer[SessionState](c, rb)
}

func (c FfiConverterSessionState) Lower(value SessionState) C.RustBuffer {
	return LowerIntoRustBuffer[SessionState](c, value)
}

func (c FfiConverterSessionState) LowerExternal(value SessionState) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[SessionState](c, value))
}
func (FfiConverterSessionState) Read(reader io.Reader) SessionState {
	id := readInt32(reader)
	return SessionState(id)
}

func (FfiConverterSessionState) Write(writer io.Writer, value SessionState) {
	writeInt32(writer, int32(value))
}

type FfiDestroyerSessionState struct{}

func (_ FfiDestroyerSessionState) Destroy(value SessionState) {
}

type FfiConverterOptionalUint32 struct{}

var FfiConverterOptionalUint32INSTANCE = FfiConverterOptionalUint32{}

func (c FfiConverterOptionalUint32) Lift(rb RustBufferI) *uint32 {
	return LiftFromRustBuffer[*uint32](c, rb)
}

func (_ FfiConverterOptionalUint32) Read(reader io.Reader) *uint32 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint32INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint32) Lower(value *uint32) C.RustBuffer {
	return LowerIntoRustBuffer[*uint32](c, value)
}

func (c FfiConverterOptionalUint32) LowerExternal(value *uint32) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint32](c, value))
}

func (_ FfiConverterOptionalUint32) Write(writer io.Writer, value *uint32) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint32INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint32 struct{}

func (_ FfiDestroyerOptionalUint32) Destroy(value *uint32) {
	if value != nil {
		FfiDestroyerUint32{}.Destroy(*value)
	}
}

type FfiConverterOptionalUint64 struct{}

var FfiConverterOptionalUint64INSTANCE = FfiConverterOptionalUint64{}

func (c FfiConverterOptionalUint64) Lift(rb RustBufferI) *uint64 {
	return LiftFromRustBuffer[*uint64](c, rb)
}

func (_ FfiConverterOptionalUint64) Read(reader io.Reader) *uint64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterUint64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalUint64) Lower(value *uint64) C.RustBuffer {
	return LowerIntoRustBuffer[*uint64](c, value)
}

func (c FfiConverterOptionalUint64) LowerExternal(value *uint64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*uint64](c, value))
}

func (_ FfiConverterOptionalUint64) Write(writer io.Writer, value *uint64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterUint64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalUint64 struct{}

func (_ FfiDestroyerOptionalUint64) Destroy(value *uint64) {
	if value != nil {
		FfiDestroyerUint64{}.Destroy(*value)
	}
}

type FfiConverterOptionalInt64 struct{}

var FfiConverterOptionalInt64INSTANCE = FfiConverterOptionalInt64{}

func (c FfiConverterOptionalInt64) Lift(rb RustBufferI) *int64 {
	return LiftFromRustBuffer[*int64](c, rb)
}

func (_ FfiConverterOptionalInt64) Read(reader io.Reader) *int64 {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterInt64INSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalInt64) Lower(value *int64) C.RustBuffer {
	return LowerIntoRustBuffer[*int64](c, value)
}

func (c FfiConverterOptionalInt64) LowerExternal(value *int64) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*int64](c, value))
}

func (_ FfiConverterOptionalInt64) Write(writer io.Writer, value *int64) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterInt64INSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalInt64 struct{}

func (_ FfiDestroyerOptionalInt64) Destroy(value *int64) {
	if value != nil {
		FfiDestroyerInt64{}.Destroy(*value)
	}
}

type FfiConverterOptionalBool struct{}

var FfiConverterOptionalBoolINSTANCE = FfiConverterOptionalBool{}

func (c FfiConverterOptionalBool) Lift(rb RustBufferI) *bool {
	return LiftFromRustBuffer[*bool](c, rb)
}

func (_ FfiConverterOptionalBool) Read(reader io.Reader) *bool {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBoolINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBool) Lower(value *bool) C.RustBuffer {
	return LowerIntoRustBuffer[*bool](c, value)
}

func (c FfiConverterOptionalBool) LowerExternal(value *bool) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*bool](c, value))
}

func (_ FfiConverterOptionalBool) Write(writer io.Writer, value *bool) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBoolINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBool struct{}

func (_ FfiDestroyerOptionalBool) Destroy(value *bool) {
	if value != nil {
		FfiDestroyerBool{}.Destroy(*value)
	}
}

type FfiConverterOptionalString struct{}

var FfiConverterOptionalStringINSTANCE = FfiConverterOptionalString{}

func (c FfiConverterOptionalString) Lift(rb RustBufferI) *string {
	return LiftFromRustBuffer[*string](c, rb)
}

func (_ FfiConverterOptionalString) Read(reader io.Reader) *string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalString) Lower(value *string) C.RustBuffer {
	return LowerIntoRustBuffer[*string](c, value)
}

func (c FfiConverterOptionalString) LowerExternal(value *string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*string](c, value))
}

func (_ FfiConverterOptionalString) Write(writer io.Writer, value *string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalString struct{}

func (_ FfiDestroyerOptionalString) Destroy(value *string) {
	if value != nil {
		FfiDestroyerString{}.Destroy(*value)
	}
}

type FfiConverterOptionalBytes struct{}

var FfiConverterOptionalBytesINSTANCE = FfiConverterOptionalBytes{}

func (c FfiConverterOptionalBytes) Lift(rb RustBufferI) *[]byte {
	return LiftFromRustBuffer[*[]byte](c, rb)
}

func (_ FfiConverterOptionalBytes) Read(reader io.Reader) *[]byte {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterBytesINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalBytes) Lower(value *[]byte) C.RustBuffer {
	return LowerIntoRustBuffer[*[]byte](c, value)
}

func (c FfiConverterOptionalBytes) LowerExternal(value *[]byte) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]byte](c, value))
}

func (_ FfiConverterOptionalBytes) Write(writer io.Writer, value *[]byte) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterBytesINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalBytes struct{}

func (_ FfiDestroyerOptionalBytes) Destroy(value *[]byte) {
	if value != nil {
		FfiDestroyerBytes{}.Destroy(*value)
	}
}

type FfiConverterOptionalLoaded struct{}

var FfiConverterOptionalLoadedINSTANCE = FfiConverterOptionalLoaded{}

func (c FfiConverterOptionalLoaded) Lift(rb RustBufferI) *Loaded {
	return LiftFromRustBuffer[*Loaded](c, rb)
}

func (_ FfiConverterOptionalLoaded) Read(reader io.Reader) *Loaded {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterLoadedINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalLoaded) Lower(value *Loaded) C.RustBuffer {
	return LowerIntoRustBuffer[*Loaded](c, value)
}

func (c FfiConverterOptionalLoaded) LowerExternal(value *Loaded) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*Loaded](c, value))
}

func (_ FfiConverterOptionalLoaded) Write(writer io.Writer, value *Loaded) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterLoadedINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalLoaded struct{}

func (_ FfiDestroyerOptionalLoaded) Destroy(value *Loaded) {
	if value != nil {
		FfiDestroyerLoaded{}.Destroy(*value)
	}
}

type FfiConverterOptionalManagement struct{}

var FfiConverterOptionalManagementINSTANCE = FfiConverterOptionalManagement{}

func (c FfiConverterOptionalManagement) Lift(rb RustBufferI) *Management {
	return LiftFromRustBuffer[*Management](c, rb)
}

func (_ FfiConverterOptionalManagement) Read(reader io.Reader) *Management {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterManagementINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalManagement) Lower(value *Management) C.RustBuffer {
	return LowerIntoRustBuffer[*Management](c, value)
}

func (c FfiConverterOptionalManagement) LowerExternal(value *Management) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*Management](c, value))
}

func (_ FfiConverterOptionalManagement) Write(writer io.Writer, value *Management) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterManagementINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalManagement struct{}

func (_ FfiDestroyerOptionalManagement) Destroy(value *Management) {
	if value != nil {
		FfiDestroyerManagement{}.Destroy(*value)
	}
}

type FfiConverterOptionalPolicyInput struct{}

var FfiConverterOptionalPolicyInputINSTANCE = FfiConverterOptionalPolicyInput{}

func (c FfiConverterOptionalPolicyInput) Lift(rb RustBufferI) *PolicyInput {
	return LiftFromRustBuffer[*PolicyInput](c, rb)
}

func (_ FfiConverterOptionalPolicyInput) Read(reader io.Reader) *PolicyInput {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterPolicyInputINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalPolicyInput) Lower(value *PolicyInput) C.RustBuffer {
	return LowerIntoRustBuffer[*PolicyInput](c, value)
}

func (c FfiConverterOptionalPolicyInput) LowerExternal(value *PolicyInput) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*PolicyInput](c, value))
}

func (_ FfiConverterOptionalPolicyInput) Write(writer io.Writer, value *PolicyInput) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterPolicyInputINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalPolicyInput struct{}

func (_ FfiDestroyerOptionalPolicyInput) Destroy(value *PolicyInput) {
	if value != nil {
		FfiDestroyerPolicyInput{}.Destroy(*value)
	}
}

type FfiConverterOptionalPolicySummary struct{}

var FfiConverterOptionalPolicySummaryINSTANCE = FfiConverterOptionalPolicySummary{}

func (c FfiConverterOptionalPolicySummary) Lift(rb RustBufferI) *PolicySummary {
	return LiftFromRustBuffer[*PolicySummary](c, rb)
}

func (_ FfiConverterOptionalPolicySummary) Read(reader io.Reader) *PolicySummary {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterPolicySummaryINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalPolicySummary) Lower(value *PolicySummary) C.RustBuffer {
	return LowerIntoRustBuffer[*PolicySummary](c, value)
}

func (c FfiConverterOptionalPolicySummary) LowerExternal(value *PolicySummary) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*PolicySummary](c, value))
}

func (_ FfiConverterOptionalPolicySummary) Write(writer io.Writer, value *PolicySummary) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterPolicySummaryINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalPolicySummary struct{}

func (_ FfiDestroyerOptionalPolicySummary) Destroy(value *PolicySummary) {
	if value != nil {
		FfiDestroyerPolicySummary{}.Destroy(*value)
	}
}

type FfiConverterOptionalSessionInfo struct{}

var FfiConverterOptionalSessionInfoINSTANCE = FfiConverterOptionalSessionInfo{}

func (c FfiConverterOptionalSessionInfo) Lift(rb RustBufferI) *SessionInfo {
	return LiftFromRustBuffer[*SessionInfo](c, rb)
}

func (_ FfiConverterOptionalSessionInfo) Read(reader io.Reader) *SessionInfo {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSessionInfoINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSessionInfo) Lower(value *SessionInfo) C.RustBuffer {
	return LowerIntoRustBuffer[*SessionInfo](c, value)
}

func (c FfiConverterOptionalSessionInfo) LowerExternal(value *SessionInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*SessionInfo](c, value))
}

func (_ FfiConverterOptionalSessionInfo) Write(writer io.Writer, value *SessionInfo) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSessionInfoINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSessionInfo struct{}

func (_ FfiDestroyerOptionalSessionInfo) Destroy(value *SessionInfo) {
	if value != nil {
		FfiDestroyerSessionInfo{}.Destroy(*value)
	}
}

type FfiConverterOptionalSequenceString struct{}

var FfiConverterOptionalSequenceStringINSTANCE = FfiConverterOptionalSequenceString{}

func (c FfiConverterOptionalSequenceString) Lift(rb RustBufferI) *[]string {
	return LiftFromRustBuffer[*[]string](c, rb)
}

func (_ FfiConverterOptionalSequenceString) Read(reader io.Reader) *[]string {
	if readInt8(reader) == 0 {
		return nil
	}
	temp := FfiConverterSequenceStringINSTANCE.Read(reader)
	return &temp
}

func (c FfiConverterOptionalSequenceString) Lower(value *[]string) C.RustBuffer {
	return LowerIntoRustBuffer[*[]string](c, value)
}

func (c FfiConverterOptionalSequenceString) LowerExternal(value *[]string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[*[]string](c, value))
}

func (_ FfiConverterOptionalSequenceString) Write(writer io.Writer, value *[]string) {
	if value == nil {
		writeInt8(writer, 0)
	} else {
		writeInt8(writer, 1)
		FfiConverterSequenceStringINSTANCE.Write(writer, *value)
	}
}

type FfiDestroyerOptionalSequenceString struct{}

func (_ FfiDestroyerOptionalSequenceString) Destroy(value *[]string) {
	if value != nil {
		FfiDestroyerSequenceString{}.Destroy(*value)
	}
}

type FfiConverterSequenceString struct{}

var FfiConverterSequenceStringINSTANCE = FfiConverterSequenceString{}

func (c FfiConverterSequenceString) Lift(rb RustBufferI) []string {
	return LiftFromRustBuffer[[]string](c, rb)
}

func (c FfiConverterSequenceString) Read(reader io.Reader) []string {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]string, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterStringINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceString) Lower(value []string) C.RustBuffer {
	return LowerIntoRustBuffer[[]string](c, value)
}

func (c FfiConverterSequenceString) LowerExternal(value []string) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]string](c, value))
}

func (c FfiConverterSequenceString) Write(writer io.Writer, value []string) {
	if len(value) > math.MaxInt32 {
		panic("[]string is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterStringINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceString struct{}

func (FfiDestroyerSequenceString) Destroy(sequence []string) {
	for _, value := range sequence {
		FfiDestroyerString{}.Destroy(value)
	}
}

type FfiConverterSequenceContentBlock struct{}

var FfiConverterSequenceContentBlockINSTANCE = FfiConverterSequenceContentBlock{}

func (c FfiConverterSequenceContentBlock) Lift(rb RustBufferI) []ContentBlock {
	return LiftFromRustBuffer[[]ContentBlock](c, rb)
}

func (c FfiConverterSequenceContentBlock) Read(reader io.Reader) []ContentBlock {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ContentBlock, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterContentBlockINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceContentBlock) Lower(value []ContentBlock) C.RustBuffer {
	return LowerIntoRustBuffer[[]ContentBlock](c, value)
}

func (c FfiConverterSequenceContentBlock) LowerExternal(value []ContentBlock) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]ContentBlock](c, value))
}

func (c FfiConverterSequenceContentBlock) Write(writer io.Writer, value []ContentBlock) {
	if len(value) > math.MaxInt32 {
		panic("[]ContentBlock is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterContentBlockINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceContentBlock struct{}

func (FfiDestroyerSequenceContentBlock) Destroy(sequence []ContentBlock) {
	for _, value := range sequence {
		FfiDestroyerContentBlock{}.Destroy(value)
	}
}

type FfiConverterSequenceDiagnostic struct{}

var FfiConverterSequenceDiagnosticINSTANCE = FfiConverterSequenceDiagnostic{}

func (c FfiConverterSequenceDiagnostic) Lift(rb RustBufferI) []Diagnostic {
	return LiftFromRustBuffer[[]Diagnostic](c, rb)
}

func (c FfiConverterSequenceDiagnostic) Read(reader io.Reader) []Diagnostic {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]Diagnostic, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterDiagnosticINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceDiagnostic) Lower(value []Diagnostic) C.RustBuffer {
	return LowerIntoRustBuffer[[]Diagnostic](c, value)
}

func (c FfiConverterSequenceDiagnostic) LowerExternal(value []Diagnostic) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]Diagnostic](c, value))
}

func (c FfiConverterSequenceDiagnostic) Write(writer io.Writer, value []Diagnostic) {
	if len(value) > math.MaxInt32 {
		panic("[]Diagnostic is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterDiagnosticINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceDiagnostic struct{}

func (FfiDestroyerSequenceDiagnostic) Destroy(sequence []Diagnostic) {
	for _, value := range sequence {
		FfiDestroyerDiagnostic{}.Destroy(value)
	}
}

type FfiConverterSequencePolicyPreset struct{}

var FfiConverterSequencePolicyPresetINSTANCE = FfiConverterSequencePolicyPreset{}

func (c FfiConverterSequencePolicyPreset) Lift(rb RustBufferI) []PolicyPreset {
	return LiftFromRustBuffer[[]PolicyPreset](c, rb)
}

func (c FfiConverterSequencePolicyPreset) Read(reader io.Reader) []PolicyPreset {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]PolicyPreset, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterPolicyPresetINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequencePolicyPreset) Lower(value []PolicyPreset) C.RustBuffer {
	return LowerIntoRustBuffer[[]PolicyPreset](c, value)
}

func (c FfiConverterSequencePolicyPreset) LowerExternal(value []PolicyPreset) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]PolicyPreset](c, value))
}

func (c FfiConverterSequencePolicyPreset) Write(writer io.Writer, value []PolicyPreset) {
	if len(value) > math.MaxInt32 {
		panic("[]PolicyPreset is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterPolicyPresetINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequencePolicyPreset struct{}

func (FfiDestroyerSequencePolicyPreset) Destroy(sequence []PolicyPreset) {
	for _, value := range sequence {
		FfiDestroyerPolicyPreset{}.Destroy(value)
	}
}

type FfiConverterSequenceSessionInfo struct{}

var FfiConverterSequenceSessionInfoINSTANCE = FfiConverterSequenceSessionInfo{}

func (c FfiConverterSequenceSessionInfo) Lift(rb RustBufferI) []SessionInfo {
	return LiftFromRustBuffer[[]SessionInfo](c, rb)
}

func (c FfiConverterSequenceSessionInfo) Read(reader io.Reader) []SessionInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]SessionInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterSessionInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceSessionInfo) Lower(value []SessionInfo) C.RustBuffer {
	return LowerIntoRustBuffer[[]SessionInfo](c, value)
}

func (c FfiConverterSequenceSessionInfo) LowerExternal(value []SessionInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]SessionInfo](c, value))
}

func (c FfiConverterSequenceSessionInfo) Write(writer io.Writer, value []SessionInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]SessionInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterSessionInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceSessionInfo struct{}

func (FfiDestroyerSequenceSessionInfo) Destroy(sequence []SessionInfo) {
	for _, value := range sequence {
		FfiDestroyerSessionInfo{}.Destroy(value)
	}
}

type FfiConverterSequenceToolInfo struct{}

var FfiConverterSequenceToolInfoINSTANCE = FfiConverterSequenceToolInfo{}

func (c FfiConverterSequenceToolInfo) Lift(rb RustBufferI) []ToolInfo {
	return LiftFromRustBuffer[[]ToolInfo](c, rb)
}

func (c FfiConverterSequenceToolInfo) Read(reader io.Reader) []ToolInfo {
	length := readInt32(reader)
	if length == 0 {
		return nil
	}
	result := make([]ToolInfo, 0, length)
	for i := int32(0); i < length; i++ {
		result = append(result, FfiConverterToolInfoINSTANCE.Read(reader))
	}
	return result
}

func (c FfiConverterSequenceToolInfo) Lower(value []ToolInfo) C.RustBuffer {
	return LowerIntoRustBuffer[[]ToolInfo](c, value)
}

func (c FfiConverterSequenceToolInfo) LowerExternal(value []ToolInfo) ExternalCRustBuffer {
	return RustBufferFromC(LowerIntoRustBuffer[[]ToolInfo](c, value))
}

func (c FfiConverterSequenceToolInfo) Write(writer io.Writer, value []ToolInfo) {
	if len(value) > math.MaxInt32 {
		panic("[]ToolInfo is too large to fit into Int32")
	}

	writeInt32(writer, int32(len(value)))
	for _, item := range value {
		FfiConverterToolInfoINSTANCE.Write(writer, item)
	}
}

type FfiDestroyerSequenceToolInfo struct{}

func (FfiDestroyerSequenceToolInfo) Destroy(sequence []ToolInfo) {
	for _, value := range sequence {
		FfiDestroyerToolInfo{}.Destroy(value)
	}
}

const (
	uniffiRustFuturePollReady      int8 = 0
	uniffiRustFuturePollMaybeReady int8 = 1
)

type rustFuturePollFunc func(C.uint64_t, C.UniffiRustFutureContinuationCallback, C.uint64_t)
type rustFutureCompleteFunc[T any] func(C.uint64_t, *C.RustCallStatus) T
type rustFutureFreeFunc func(C.uint64_t)

//export computeruse_uniffiFutureContinuationCallback
func computeruse_uniffiFutureContinuationCallback(data C.uint64_t, pollResult C.int8_t) {
	h := cgo.Handle(uintptr(data))
	waiter := h.Value().(chan int8)
	waiter <- int8(pollResult)
}

func uniffiRustCallAsync[E any, T any, F any](
	errConverter BufReader[E],
	completeFunc rustFutureCompleteFunc[F],
	liftFunc func(F) T,
	rustFuture C.uint64_t,
	pollFunc rustFuturePollFunc,
	freeFunc rustFutureFreeFunc,
) (T, E) {
	defer freeFunc(rustFuture)

	pollResult := int8(-1)
	waiter := make(chan int8, 1)

	chanHandle := cgo.NewHandle(waiter)
	defer chanHandle.Delete()

	for pollResult != uniffiRustFuturePollReady {
		pollFunc(
			rustFuture,
			(C.UniffiRustFutureContinuationCallback)(C.computeruse_uniffiFutureContinuationCallback),
			C.uint64_t(chanHandle),
		)
		pollResult = <-waiter
	}

	var goValue T
	ffiValue, err := rustCallWithError(errConverter, func(status *C.RustCallStatus) F {
		return completeFunc(rustFuture, status)
	})
	if value := reflect.ValueOf(err); value.IsValid() && !value.IsZero() {
		return goValue, err
	}
	return liftFunc(ffiValue), err
}

//export computeruse_uniffiFreeGorutine
func computeruse_uniffiFreeGorutine(data C.uint64_t) {
	handle := cgo.Handle(uintptr(data))
	defer handle.Delete()

	guard := handle.Value().(chan struct{})
	guard <- struct{}{}
}

// The SDK's version.
func SdkVersion() string {
	return FfiConverterStringINSTANCE.Lift(rustCall(func(_uniffiStatus *C.RustCallStatus) RustBufferI {
		return GoRustBuffer{
			inner: C.uniffi_computeruse_fn_func_sdk_version(_uniffiStatus),
		}
	}))
}
