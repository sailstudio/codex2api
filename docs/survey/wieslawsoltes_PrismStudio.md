# Prism Studio

A runnable, Blend-inspired visual interface editor in plain HTML, CSS and JavaScript. No React, no bundler dependency, no CDN, no application server, and no proprietary Microsoft assets.

**Release:** 0.1.0. This is an original web editor with a working design/animation workflow. It is not a complete implementation of Blend, WPF, WinUI or Avalonia, and is not represented as a production-certified replacement for them.

[Open Prism Studio](https://wieslawsoltes.github.io/PrismStudio/) · [Build and deployment](https://github.com/wieslawsoltes/PrismStudio/actions/workflows/pages.yml)

## Run

Node 20 or newer:

```sh
npm start
# Open http://127.0.0.1:4173
```

There is no `npm install` step. The server uses only Node built-ins.

Alternatively:

```sh
python3 -m http.server 4173
# Open http://localhost:4173
```

`dist/index.html` is a prebuilt, self-contained application. It also works as a directly opened local file when browser policy permits. For the most predictable GPU and storage behavior, serve the directory locally or host it on HTTPS. WebGPU needs a supporting browser and a secure context; Canvas 2D is selected automatically when WebGPU initialization fails. The toolbar always reports the actual backend. `?canvas=1` explicitly selects the fallback.

## Included workflows

### Visual design

- An editable 116-element sample dashboard, not a background image.
- Tool rail, searchable/draggable Assets, Projects, Objects and Timeline, Properties, Events, States and Resources panels.
- Rectangle, ellipse, line, TextBlock, Button, TextBox, CheckBox, Image, Border, Canvas, uniform Grid and StackPanel elements.
- Click and multiselect; marquee selection; direct selection; drag; eight resize handles; center-origin rotation; scale properties.
- Shift-constrained drawing, movement and rotation; keyboard nudging; edge/center snapping; optional eight-unit grid; rulers.
- Cursor-anchored zoom, hand/Space panning, fit artboard and fit selection.
- Tree visibility/locking, same-parent grouping, ungrouping, drag reparenting, z-order changes and alignment.
- Double-click text editing, typography controls, fill/stroke colors, opacity, corner radii and two-stop linear gradients.
- Embedded local raster-image import. No remote image fetches are performed.

### Animation and interaction

- A project storyboard with multiple property tracks.
- Keyframe insertion, direct time dragging, deletion, scrubbing, playback, looping and duration changes.
- Linear, smoothstep ease-in-out, cubic ease-in, cubic ease-out and discrete hold evaluation.
- Recording mode writes numerical property edits into keyframes instead of mutating authored base properties.
- Visual states store property overrides separately from the base scene.
- Interactive preview supports checkbox toggles, text entry, click messages, state changes and storyboard playback.
- Brushes can be saved in the project resource palette and applied to selections.

### Documents and exports

- Transactional undo/redo: gestures form one undo unit, failed edits roll back, and new changes invalidate the redo branch.
- Browser-local autosave with explicit quota/error reporting.
- Lossless `.prism` project JSON with scene, images, resources, states and animation data.
- XAML design, split and source views; explicit Apply; parser diagnostics; atomic replacement only after successful validation.
- WPF visual-subset XAML import/export, including names, brushes, transforms and supported keyframe storyboard tracks.
- Real PNG export at 2× resolution.
- Standalone interactive HTML/CSS/JavaScript export with no external dependencies.
- A command palette and keyboard shortcuts dialog.

## Architecture

| File | Responsibility |
| --- | --- |
| `src/core.js` | Retained scene model, affine geometry, layout, validation, picking, transactions, history, keyframes, states and snapping |
| `src/renderer.js` | WebGPU instance pipeline, WGSL, raster atlas, display-list cache, viewport culling, Canvas 2D fallback and PNG export |
| `src/xaml.js` | Restricted XML import, WPF subset serialization, resources, transforms and keyframe conversion |
| `src/sample.js` | Editable sample document and storyboard |
| `src/app.js` | Editor state, gestures, tools, inspector, timeline, commands, file operations and HTML runtime export |
| `style.css` | UI design tokens and desktop editor layout |
| `build.mjs` | Dependency-free single-file packager |
| `server.mjs` | Loopback static server using Node built-ins |

### GPU pipeline

The actual artboard is rendered into a WebGPU canvas, not drawn behind HTML elements that pretend to be GPU content. Each drawing instance occupies nine `vec4<f32>` values: **144 bytes**, 16-byte aligned. Its descriptor includes a 2D affine transform, local dimensions, fill/stroke brushes, geometry parameters, opacity, atlas UV coordinates, a world-space clip rectangle and gradient parameters.

The vertex shader synthesizes a six-vertex quad from `vertex_index` and reads descriptors by `instance_index`. The fragment shader evaluates analytic rounded-rectangle/ellipse coverage, gradient brushes and atlas content. Premultiplied-alpha blending composites the ordered instances. A visible scene uses one instanced draw in this pipeline. Capacity-grown buffers are reused; camera data resides in a 32-byte uniform buffer.

Text is browser-rasterized and cached in a 4096×4096 RGBA atlas; images use the same texture. Typography is therefore **not** a GPU font-outline engine or a native WPF text formatter. The atlas consumes approximately 64 MiB of GPU texture memory, in addition to browser-side raster storage. UI chrome is ordinary DOM; rulers, guides and selection adorners use a separate Canvas 2D overlay. PNG export deliberately uses the equivalent Canvas 2D path.

Scene flattening is cached by document revision, animation time and visual state. It resolves nested affine transforms, inherited visibility/opacity, clipping and container layout. The viewport culls offscreen visuals. Rendering is demand-driven while idle and advances through requestAnimationFrame during playback. The status-bar time is **CPU-side preparation/submission time**, not GPU execution time or an end-to-end frame-rate benchmark.

### Model semantics

Authored node properties are separate from evaluated animation and visual-state values. Rendering does not overwrite authored coordinates. Reparenting preserves the base world transform when the transform can be represented without shear. Auto-layout containers deliberately recompute child positions.

History uses document snapshots, capped at 100 transactions. It is simple and atomic but is not a structural-sharing, disk-backed undo log. Very large image-heavy documents can consume significant history memory. Scene input limits are 10,000 nodes, 64 nesting levels and finite numeric fields; file import is limited to 20 MB, and individual image import to 8 MB. Browser storage has its own quota and can be lower than these limits.

## Compatibility boundaries

These limits are deliberate and must not be confused with full Blend compatibility:

- No .NET runtime, dependency-property engine, data binding, code-behind execution, project compilation, debugger, NuGet restoration or native WPF/WinUI/Avalonia control implementation.
- No arbitrary custom controls, ControlTemplate/Style evaluation, complete resource lookup/markup-extension execution, vector path editor, Boolean geometry engine, effects, 3D scene editor or collaborative editing.
- `Grid` is a uniform grid, not WPF's general Auto/Star row/column measure/arrange implementation. StackPanel is an explicit-size layout. Full margin, alignment, min/max sizing and layout-invalidation semantics are not implemented.
- Rounded containers export through a Border/Canvas composition. Buttons and input controls are custom editor visuals; XAML export uses native WPF control types, whose default templates will differ.
- StackPanel gaps/padding and uniform-grid gaps are editor features, not a lossless representation of every native WPF layout property. Use project JSON for exact project recovery.
- XAML gradients support two stops; storyboard export covers X/Y, width/height, opacity, rotation and scale. Runtime easing is approximated by KeySpline values in exported WPF XAML; it is not bit-identical.
- Embedded images, runtime events and visual-state metadata are preserved in `.prism` and HTML export. Embedded images become documented placeholders in XAML. WPF ImageSource integration is not implemented.
- Imported XML is parsed, never evaluated as code. DTD/entity declarations are rejected. Unsupported visual types fail with diagnostics. Unsupported property elements are reported rather than executed.
- Nested clipping is axis-aligned in world space; rotated clips use bounding rectangles. Corner clipping is not a stencil-based rounded clip stack.
- Text rasterization uses browser fonts/metrics. The renderer is not an independent Unicode shaping engine, DirectWrite clone or resolution-independent vector-font renderer.
- Ungrouping transformed or animated/stateful groups is guarded to prevent silent data loss. Reset transforms/remove the group-level tracks or overrides first. Reparenting changes the parent coordinate space; existing authored animation tracks remain local to their element.
- Resource application copies a brush value. It does not establish a live DynamicResource dependency.
- One open design document and one storyboard are supported. The workspace is desktop-oriented, with a 980px minimum layout width.
- WebGPU device loss falls back to Canvas 2D. Atlas overflow is reported. There is no GPU timestamp-query benchmark suite or automatic GPU-device re-creation loop.

## Validation performed

```sh
npm test
npm run build
python3 tests/browser_smoke.py
```

The JavaScript unit suite uses Node's built-in test runner. The browser suite needs Python Playwright and Chromium as development-only test dependencies; they are not application dependencies.

For a normal browser run against the development server:

```sh
PRISM_URL=http://127.0.0.1:4173 CHROMIUM=/path/to/chromium \
  python3 tests/browser_smoke.py
```

The supplied execution report is `tests/browser-results.json`. The artifact build was exercised in Chromium using a synthetic local document because the environment blocks navigation to local URLs. **That run used Canvas 2D, not WebGPU.** The GPU shaders, pipeline and recovery logic are implemented, but successful hardware execution and hardware performance are not verified by that report. The browser suite records whether WebGPU was actually exercised; it never substitutes a simulated “WebGPU enabled” indicator.

The browser checks cover property editing, real pointer drawing/resizing/rotation, undo/redo, text, XAML entities/newlines and storyboard round-trip, split view, invalid-XAML atomicity, animation recording/playback, visual states, bitmap export and execution of exported HTML.

## Static deployment

Live application: https://wieslawsoltes.github.io/PrismStudio/

`dist/index.html` is all a static host needs. The repository uses branch-based GitHub Pages publishing: **Settings → Pages → Deploy from a branch → gh-pages → / (root)**. Keep that publishing source; this workflow does not require switching the source to GitHub Actions.

The included `.github/workflows/pages.yml` runs the kernel tests and builds the single-file application for pushes and pull requests to `main`. Only successful, trusted `main` runs may publish. The publish job verifies the build's SHA-256 checksum, updates `index.html` and `.nojekyll` on `gh-pages` with a fast-forward commit when needed, and requests GitHub's native Pages build. The native Pages workflow performs the actual deployment on the configured publishing branch and retains its environment protections. The job then checks the Pages build result and verifies the live HTML against the tested artifact's SHA-256 checksum.

No personal access token or third-party deployment service is required. Pull requests have read-only repository permissions and never publish. The publish job has only repository contents and Pages write permissions. Repository history is not force-pushed, and source files remain on `main`.

## Reference specifications

- Microsoft Blend workspace: https://learn.microsoft.com/en-us/visualstudio/xaml-tools/creating-a-ui-by-using-blend-for-visual-studio
- Microsoft XAML designer overview: https://learn.microsoft.com/en-us/visualstudio/xaml-tools/designing-xaml-in-visual-studio
- WebGPU specification: https://www.w3.org/TR/webgpu/
- GitHub Pages publishing sources: https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site
- GitHub Pages build API: https://docs.github.com/en/rest/pages/pages#request-a-github-pages-build

MIT licensed. Microsoft Blend, WPF, WinUI and Avalonia are referenced for interoperability/context only; Prism Studio is an independent implementation.
