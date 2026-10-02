//! `#[derive(UniffiBuilder)]`: a typed builder for a `uniffi::Record` that is
//! itself a `uniffi::Object`, so it exists in every language UniFFI
//! generates bindings for.
//!
//! For a record `Foo` it generates `FooBuilder` with
//!
//! - `FooBuilder::new()` (a UniFFI constructor) and `Foo::builder()`,
//! - one setter per field, `fn field(&self, value: T) -> Arc<FooBuilder>`,
//!   which returns a new builder and leaves its receiver as it was (no
//!   interior mutability for a foreign language to trip over),
//! - `fn build(&self) -> Result<Foo, E>`.
//!
//! A field whose type is spelled `Option<T>` (or `std::option::Option<T>`,
//! `core::option::Option<T>`) may be left out, and its setter takes `T`. Any
//! other field is required: leaving it out makes `build()` answer
//! `E::missing("Foo", "field")`. Type aliases of `Option` are not seen
//! through: the derive reads syntax only.
//!
//! `E` is named by `#[uniffi_builder(path::to::Error)]` and must have
//! `fn missing(record_type: &str, field: &str) -> Self`.
//!
//! The builder's `Debug` prints no field, so a record that carries a secret
//! does not leak it through its builder.

use proc_macro::TokenStream;
use quote::{format_ident, quote};
use syn::{
    parse_macro_input, Data, DeriveInput, Fields, GenericArgument, Path, PathArguments, Type,
};

#[proc_macro_derive(UniffiBuilder, attributes(uniffi_builder))]
pub fn derive_uniffi_builder(input: TokenStream) -> TokenStream {
    let input = parse_macro_input!(input as DeriveInput);
    match expand(input) {
        Ok(tokens) => tokens.into(),
        Err(error) => error.to_compile_error().into(),
    }
}

fn expand(input: DeriveInput) -> syn::Result<proc_macro2::TokenStream> {
    let record = &input.ident;
    let visibility = &input.vis;
    let builder = format_ident!("{record}Builder");
    let record_name = record.to_string();

    if !input.generics.params.is_empty() {
        return Err(syn::Error::new_spanned(
            &input.generics,
            "UniffiBuilder does not support generic records",
        ));
    }

    let error: Path = input
        .attrs
        .iter()
        .find(|attribute| attribute.path().is_ident("uniffi_builder"))
        .ok_or_else(|| {
            syn::Error::new_spanned(
                record,
                "UniffiBuilder needs #[uniffi_builder(path::to::BuildError)]",
            )
        })?
        .parse_args()?;

    let Data::Struct(data) = &input.data else {
        return Err(syn::Error::new_spanned(
            record,
            "UniffiBuilder is for structs with named fields",
        ));
    };
    let Fields::Named(fields) = &data.fields else {
        return Err(syn::Error::new_spanned(
            record,
            "UniffiBuilder is for structs with named fields",
        ));
    };

    let mut slots = Vec::new();
    let mut setters = Vec::new();
    let mut assignments = Vec::new();
    let mut names = Vec::new();

    for field in &fields.named {
        let name = field.ident.as_ref().expect("named field");
        let field_name = name.to_string();
        let docs: Vec<_> = field
            .attrs
            .iter()
            .filter(|attribute| attribute.path().is_ident("doc"))
            .collect();
        names.push(name);

        // Every slot is an Option of what the setter takes.
        let (value, required) = match option_inner(&field.ty) {
            Some(inner) => (inner.clone(), false),
            None => (field.ty.clone(), true),
        };
        slots.push(quote! { #name: ::std::option::Option<#value> });
        setters.push(quote! {
            #(#docs)*
            pub fn #name(&self, value: #value) -> ::std::sync::Arc<Self> {
                let mut next = ::std::clone::Clone::clone(self);
                next.#name = ::std::option::Option::Some(value);
                ::std::sync::Arc::new(next)
            }
        });
        assignments.push(if required {
            quote! {
                #name: self.#name.clone().ok_or_else(|| <#error>::missing(#record_name, #field_name))?
            }
        } else {
            quote! { #name: self.#name.clone() }
        });
    }

    let builder_doc = format!(
        "Builds a [`{record_name}`]. Each setter returns a new builder; the receiver is unchanged."
    );
    let build_doc = format!(
        "The [`{record_name}`], or `MissingRequiredField` naming the first required field left out."
    );
    let builder_name = builder.to_string();

    Ok(quote! {
        #[doc = #builder_doc]
        #[derive(Clone, uniffi::Object)]
        #visibility struct #builder {
            #(#slots,)*
        }

        impl ::std::fmt::Debug for #builder {
            fn fmt(&self, formatter: &mut ::std::fmt::Formatter<'_>) -> ::std::fmt::Result {
                formatter.debug_struct(#builder_name).finish_non_exhaustive()
            }
        }

        #[uniffi::export]
        impl #builder {
            /// A builder with nothing set.
            #[uniffi::constructor]
            pub fn new() -> ::std::sync::Arc<Self> {
                ::std::sync::Arc::new(Self { #(#names: ::std::option::Option::None,)* })
            }

            #(#setters)*

            #[doc = #build_doc]
            pub fn build(&self) -> ::std::result::Result<#record, #error> {
                ::std::result::Result::Ok(#record { #(#assignments,)* })
            }
        }

        impl #record {
            /// A builder for this record with nothing set.
            pub fn builder() -> ::std::sync::Arc<#builder> {
                #builder::new()
            }
        }
    })
}

/// `T` when `ty` is spelled `Option<T>`, `std::option::Option<T>` or
/// `core::option::Option<T>`.
fn option_inner(ty: &Type) -> Option<&Type> {
    let Type::Path(path) = ty else {
        return None;
    };
    if path.qself.is_some() {
        return None;
    }
    let segments: Vec<_> = path.path.segments.iter().collect();
    let spelled: Vec<String> = segments.iter().map(|s| s.ident.to_string()).collect();
    let spelled: Vec<&str> = spelled.iter().map(String::as_str).collect();
    if !matches!(
        spelled.as_slice(),
        ["Option"] | ["std", "option", "Option"] | ["core", "option", "Option"]
    ) {
        return None;
    }
    let PathArguments::AngleBracketed(arguments) = &segments.last()?.arguments else {
        return None;
    };
    if arguments.args.len() != 1 {
        return None;
    }
    match arguments.args.first()? {
        GenericArgument::Type(inner) => Some(inner),
        _ => None,
    }
}
