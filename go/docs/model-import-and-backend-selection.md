# Model imports and automatic backend selection

Status: proposed design, not an implemented feature. No running installation changes.

## User experience

Users choose a model, task, and variant when needed. LocalBase recommends a backend
and artifact bundle using best-effort compatibility detection. Detection does not
need to prove compatibility before making a recommendation. Show the selected
backend in the import form and model settings, with a way to change it.

When detection finds no plausible backend, leave the backend field blank. Users
can still download the model, select any registered backend, or register a custom
backend. Do not hide choices merely because the detector does not recognize them.

Imports accept a Hugging Face repository or file, a direct HTTP(S) URL, or a local
file. Downloading a model and being able to run it are separate outcomes.

## Three records

### Backend definition

A versioned backend definition declares an engine identifier, supported model
architectures, formats and quantization schemes, available tasks, companion-file
requirements, runtime version constraints, and platform/accelerator support.
Compatibility may depend on combinations of those fields. GGUF or ONNX alone is
not sufficient evidence of compatibility.

The adapter supplies model inspection, launch preparation, execution, health,
cancellation and unloading through the task interfaces LocalBase supports.
Downloaded model repositories do not install or execute backend extensions.
Users register custom adapters explicitly through a separate extension flow.

### Model definition

A logical model can contain several executable variants. Each variant identifies
its architecture, artifacts, format, precision, capabilities, compatible backend
versions, and required tokenizer/encoder/decoder/voice assets. Different variants
can serve different platforms without changing the logical model identity.

Facts include their evidence source: pinned upstream metadata, inspected artifact,
explicit author declaration, or verified runtime capability. Missing or conflicting
facts remain unknown. Public benchmarks and local performance measurements belong
in internal selection evidence, not descriptive catalog fields.

### Artifact source

Acquisition is independent of backend compatibility. A source adapter resolves a
downloadable artifact set, provenance, revision where available, expected sizes
and checksums where authoritative, and an optional private authentication profile.
Hugging Face revisions are resolved to immutable commits. Direct URLs remain
identified as custom sources rather than acquiring curated status.

## Import flow

1. Resolve the source and display its origin, available variants and total known
   download size. Resolve necessary companion artifacts before calling a bundle
   complete.
2. Inspect metadata and artifact headers without executing repository code.
3. Compare variant metadata with registered backend definitions and the detected
   host. Report supported, likely, unknown, or a known incompatibility, with a
   concrete reason. Missing metadata does not mean a backend is incompatible.
4. Use explicit versioned routing rules to recommend the preferred plausible
   backend and variant, unless the user has saved an override. Leave the selection
   blank if none is detected. A faster backend cannot silently remove a requested
   capability.
5. Download into staging, validate expected size and trusted checksum when supplied,
   inspect the received format/architecture, then publish the completed artifacts.
6. If no backend is selected, retain the import with a blank backend and a clear
   "Select a backend to run this model" action. Offer all registered backends and
   an "Add backend" action. Do not imply that download success is load success,
   and do not silently substitute another model or CPU execution.

## Backend overrides

The backend field is available both before downloading and after import in model
settings. Recommended backends appear first, but the full registered list remains
selectable. Display hardware availability and compatibility findings as labels
and warnings rather than using detection uncertainty to disable selection.

A user may select a backend with unknown or detected-incompatible model support.
Show the reason and allow the explicit choice. Runtime startup still reports real
errors such as missing assets, unavailable hardware, unsupported operators or
failed loading. An override does not fabricate compatibility or bypass download
integrity checks, process limits, or memory admission.

Persist selection mode separately from detection results:

- Automatic mode: recompute the best recommendation for this host; the resulting
  backend identifier may be null.
- Manual mode: retain the chosen registered backend identifier for this model on
  this device. New detection results do not replace it.

Provide "Reset to automatic". If a manually selected backend is removed, retain
its identifier as unavailable and ask the user to choose a replacement; do not
silently fall back. Device-local overrides must not become universal model
requirements when a model definition is exported or used on another device.

If switching engines requires another artifact variant, show the additional
download and preserve the user's chosen model identity. Do not silently treat
different backend formats as interchangeable.

User-specified checksum verification establishes identity against that checksum.
Computing a new hash after an unpinned URL download records identity only; it does
not establish publisher authenticity or safety.

## Custom URL warning and headers

Show this warning when confirming a custom source:

> This file comes from a custom source that LocalBase has not verified. Only
> download files you trust. A successful download does not guarantee the model is
> safe or compatible.

Allow arbitrary named request headers for authenticated downloads. Store sensitive
values in LocalBase's private credential storage; model definitions reference the
authentication profile rather than containing its secret values. Normal status,
logs, exports and errors redact credentials, including signed URL query values.

Credentials are origin-scoped. Do not forward user headers or Hugging Face tokens
to a different redirect origin unless that destination was explicitly included in
the authentication profile. Automatically generated Range headers and protocol
headers are controlled by the downloader, not overwritten by custom values.
Use TLS validation. Plain HTTP requires a separate insecure-transport warning.

## Existing implementation and required work

The current catalog already describes pinned multi-file artifacts and some
architecture-specific runtime profiles. The installer already supports resumable
staging and size/hash validation, with a Hugging Face token scoped to its origin.
Reuse those behaviors behind the acquisition interface.

The current model schema and routing do not provide the general backend registry,
logical model variants, persistent per-device backend overrides, custom backend
registration, general direct-URL import, or arbitrary private header profiles
described here. Those require implementation and integration across model
management, downloads and runtime launch planning.

## Acceptance checks

- The same logical model chooses a supported Metal/MLX variant on Apple Silicon
  and a supported CUDA/Vulkan variant on Linux using explicit rules.
- A GGUF extension does not establish architecture support. An incomplete
  multi-file model identifies its missing assets.
- Unknown metadata leaves automatic selection blank if there is no plausible
  match, while the full registered backend list remains manually selectable.
- Manual selections survive reinspection and restart. Resetting to automatic
  restores detection. A removed backend does not silently select another one.
- Users can override detection warnings and receive actual startup errors without
  bypassing integrity or resource checks. Imports can finish without a backend.
- A public or authenticated Hugging Face import resolves an immutable revision.
- A direct URL import shows the custom-source warning and supports private
  headers without exposing them in progress, errors, exports or redirects.
- An unpinned direct URL records its observed hash without claiming a verified
  publisher identity. Trusted-checksum mismatch prevents publication.
- A custom adapter can declare support through the same versioned interfaces;
  merely downloading weights never executes an extension or repository code.
- Existing OpenAI-compatible task interfaces remain unchanged for users.
