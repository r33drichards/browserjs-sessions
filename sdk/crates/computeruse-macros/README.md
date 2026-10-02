# computeruse-sdk-macros

`#[derive(UniffiBuilder)]` for the [Computer Use SDK](https://computeruse.site):
it gives a `uniffi::Record` a builder that is itself a `uniffi::Object`, so
the same typed builder exists in Rust, Python, JavaScript and Go.

```rust,ignore
#[derive(Clone, uniffi::Record, UniffiBuilder)]
#[uniffi_builder(crate::BuildError)]
pub struct CreateSessionRequest {
    pub name: Option<String>,
    pub policy_preset: Option<String>,
}

let request = CreateSessionRequest::builder().name("nightly".into()).build()?;
```

A setter returns a new builder and leaves its receiver as it was. A field
whose type is spelled `Option<T>` may be left out; any other is required,
and `build()` answers `MissingRequiredField` when one is missing.

It is an implementation detail of the `computeruse-sdk` crate and follows
its versions.
