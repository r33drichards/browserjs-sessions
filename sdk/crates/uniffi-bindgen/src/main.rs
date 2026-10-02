//! `uniffi-bindgen` at the workspace's UniFFI version. maturin runs it to
//! generate the Python module; `scripts/generate-bindings.sh` runs it too.
fn main() {
    uniffi::uniffi_bindgen_main()
}
