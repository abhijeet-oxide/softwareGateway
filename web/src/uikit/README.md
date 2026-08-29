# `uikit/` - the shared design system

**This folder is byte-identical in every tool that uses it.** Copy it whole,
in either direction, and both products keep behaving the same way. That is the
only guarantee it makes, and every rule below exists to protect it.

Today it lives in two repositories:

| repository | path |
| --- | --- |
| `configer` | `frontend/src/uikit/` |
| `softwareGateway` | `web/src/uikit/` |

A third tool adopts it by copying the folder in and doing the four things
under **Adopting it** below. Nothing else is required, and in particular no
package needs publishing: a copied folder has no version skew, no install step
and no private registry to be unreachable.

## The rule

> Nothing in this folder names a product.

No app name, no logo, no route, no domain noun. The moment a shared file says
`Configer` or `Software Gateway`, copying it over the other tool breaks that
tool, and the folder stops being copyable - which is the whole value.

What legitimately differs between two tools that look the same is exactly two
things, and both live OUTSIDE this folder:

1. **Identity** - the name, the mark, the caption, the favicon. Each app owns a
   `src/brand.ts` that exports a `BrandIdentity` (the type is declared here, in
   `brand.ts`). Shared components take it as a **prop**; the Vite plugin takes
   it as an **argument**. Copying `uikit/` never touches it.
2. **Domain vocabulary** - a parameter, a binding, a CVE, a transfer. Those are
   the app's own components, built out of these primitives.

## What is in it

| file | what it decides |
| --- | --- |
| `tokens.ts` | every colour, in light and dark, plus shape and type. **The file to edit to reskin both tools.** Also the presets and the one `ACTIVE_PRESET` switch. |
| `tokens.css` | spacing, radius, elevation, MATERIALS, motion and the type scale. The structural half of the system; also where density and font scale land. |
| `base.css` | the global rules a component library does not cover: focus ring, selection, scrollbar, tabular figures, the motion keyframes. |
| `components.css` | the primitives' styles. Plain CSS on the tokens - never utility classes, see below. |
| `antd.ts` | the same tokens, expressed as Ant Design's. |
| `ThemeProvider.tsx` | what an app mounts. Light/dark/system, density, font scale, and the `<html>` attributes plain CSS reads. |
| `color.ts` | how a component reads a colour: `c.brand`, `withAlpha`, `severity`, `envHex`. Every one of them a `var()`, never a hex. |
| `prefs.ts` | the appearance preference model, for an app that has none of its own. |
| `primitives/` | the components: card, page header, stat tile, status pill, severity tag, notice, empty state, stepper, toolbar, keycap, motion. |
| `vitePluginBrand.ts` | inlines the colour variables, the favicon and the title into `index.html` at build time. |

## What the system is built to look like

It is built to look like it belongs on the platform its operators actually sit
in front of. That is a decision with consequences, and they are worth stating so
nobody has to reverse-engineer them from the values:

- **The palette is the system palette.** The neutrals are the ones Apple's own
  surfaces use (`#1d1d1f` type, `#f5f5f7` grounds, `#d2d2d7` rules) rather than
  a generic blue-grey ramp, and the accents are systemBlue, systemGreen,
  systemOrange, systemRed and systemIndigo rather than five colours chosen one at
  a time. `envColors` follows, because an environment chip sitting beside a
  status pill in a different family is the kind of small wrongness that reads as
  two products.

- **The navigation is a MATERIAL, not a slab.** It was a block of navy - the
  house style of every enterprise dashboard and the identity of none of them. It
  is now translucent, the page's tint comes through it, everything on it is
  ordinary type, and the one saturated thing in the frame is the pill under
  wherever you are. The blur lives in `components.css`; the alpha lives in
  `tokens.ts`. **A preset may change the accent and the temperature of the
  neutrals; it may not bring the opaque chrome back.**

  A material is `backdrop-filter: saturate(180%) blur(20px)` and the saturation
  half is not optional: blur alone turns whatever is behind into grey mush, and
  pushing the colour back up is what makes it read as light through glass rather
  than as dirt. Every surface using one also states a background, because a
  browser without `backdrop-filter` has to get a solid.

- **Elevation is three ingredients, never one blur.** A hairline (a `0.5px`
  spread, so it stays one device pixel and costs no layout), a tight contact
  shadow, and a wide ambient cast. That is how a physical thing sits on a page;
  a single large soft shadow reads as a glow. The one soft-UI token that
  survived is `--el-inset`, a hairline of light along a surface's top edge -
  a real bevel, on inputs and pressed states.

- **Type is San Francisco, and it tightens as it grows.** `--tr-tight` through
  `--tr-wide` are a scale rather than a number typed at each call site: the
  tracking that keeps 11px legible makes a 24px title look like it is falling
  apart. `-webkit-font-smoothing: antialiased` AND `-moz-osx-font-smoothing:
  grayscale` are both set, because SF is drawn for grayscale antialiasing and
  renders a weight too heavy without them.

- **Radius is a ramp and the whole ramp moves together.** 6 on a small control,
  8 on a button or a field, 12 on a card, 18 on a sheet. `antd.ts` states all
  four rather than letting the component library derive them, because a 12px
  card holding a 4px button is the tell that two systems are drawing one screen.

## The chrome is shared whole

The navigation and the bar above the page are the two surfaces a person sees on
every screen of every tool, so they are the two that must not be written twice.
Two products that share a palette but each draw their own sidebar do not look
like one product; they look like two products with the same colours, which is
worse than not trying, because the difference reads as carelessness rather than
as intent.

So `SideNav` and `TopBar` own the STRUCTURE - the widths, the item heights, the
hover and active language, the collapse behaviour, the profile card at the foot,
the bar's height and how its two ends are arranged - and each app hands in only
what is genuinely its own: which entries there are, what they do, and what
belongs in its bar.

`StatusScreen` is the same argument for the moments before an app can show
anything. Every tool has them: it is checking whether its service is there, the
service did not answer, nobody is signed in. They are the first thing anybody
sees of a product and the thing they see on its worst day - exactly the wrong
place for each tool to improvise a layout.

One rule inside them is worth knowing because it was a real bug: **a lockup
aligns to itself, never to whatever it was dropped into.** The name and the
caption are different widths, so on a card that centres its text the short name
floated to the middle of the box the caption sized, and read as a mark with a
hole punched between it and its own name. `.ui-lockup-text` states
`align-items: flex-start` for that reason.

## What counts as a difference

The test is not "does it look similar" but **"could the two tools answer this
differently?"** If they could, it belongs here. Three things that failed that
test and were moved:

- **The state illustrations.** One tool showed a considered drawing where a
  service would not answer; the other showed the same sentence with nothing
  above it. A state screen is the same state screen in every tool.
- **The empty state.** One used this kit's `EmptyState` with the shared
  drawing; the other used the component library's flat default glyph, which
  belongs to no design system and is the cheapest tell that a page was
  assembled rather than built.
- **The appearance controls, copy included.** "System follows your device" is
  the same sentence in every tool. Two products that explain the same control
  in different words are two products.

The counter-example is worth stating too, because not everything shared-looking
should be shared: a file type's icon colour and a chart series' colour are
IDENTITIES, not theme. YAML is the same orange in dark mode, the same way an
environment's colour is, and they do not belong to the palette. Statuses do:
anything that means healthy / pending / failing reads a token, so it follows
the theme and a rebrand cannot miss it.

## Changing theme is an animation, not a repaint

`useTheme().toggleMode(point)` and `setTheme(pref, point)` reveal the new theme
as a circle growing out of `point`, via the View Transitions API. Pass the
click's coordinates (`pointOf(event)`) and it animates; pass nothing and it
switches instantly, which is the honest behaviour for a keyboard activation or
the operating system flipping at sunset. Reduced motion and browsers without
the API get the instant switch too.

It is HERE rather than in one app because it is not a feature, it is how this
design system changes theme - and a tool where the lights come on smoothly next
to a tool where they snap does not read as one product. Anything mounting
`ThemeProvider` gets it; the kit's own toggle already passes the point.

The one subtlety, in case it ever looks broken: the switch runs inside
`flushSync`. The View Transitions API snapshots the DOM the instant the callback
returns, so without it React has not re-rendered, the "after" snapshot is
identical to the "before" one, and the circle grows over nothing.

## Deleting CSS

`node src/uikit/check-styles.mjs` compares the stylesheets against a base
revision and fails if a class was deleted while the source still uses it. Both
repositories run it in their lint step.

It exists because extracting this kit meant deleting blocks from each app's
stylesheet, and one deletion was made by "from this selector down to that one".
Stylesheets are not sorted by topic: between the boot screen and the sign-in
screen sat a wizard and every responsive dialog rule, and the cut took all of
them with it. Nothing failed - TypeScript cannot see a class name - and it
surfaced days later as two cards that had been side by side stacking on top of
each other.

**Cut CSS by RULE, never by the distance between two markers**, and let the
check confirm it.

## Seeds are not colours

The one trap in this folder, and it cost a visible bug: **Ant Design treats
`colorPrimary`, `colorSuccess`, `colorWarning`, `colorError`, `colorInfo` and
`colorLink` as SEEDS, not as values.** Its algorithm derives the shade it
actually paints, and under the dark algorithm that is a different colour -
`#4d94e8` went in, `#4481c8` came out. So a primary button and anything written
`var(--brand)` were two different blues on the same screen, in the one place
nobody looks because both halves came from one file.

Two rules follow, and `antd.ts` enforces both:

- **The stylesheet is told what Ant will PAINT, not what it was given.**
  `resolvePalette(mode)` asks the component library (`getDesignToken`) and hands
  the answer to `renderRootCss`. Disagreement is impossible rather than merely
  unlikely.
- **Never feed a painted value back in as a seed.** It derives a second time and
  lands somewhere else again. `buildTheme` keeps `seed` and `p` as two separate
  objects for exactly this reason: seed tokens get `seed`, map tokens get `p`.

A consequence worth knowing: the dark entries in `tokens.ts` are seeds solved
BACKWARDS from the intended result, so they do not read like the colour you see
on screen, and the comment there says so.

And one hard limit, before anybody reaches for a brighter dark accent: **the
dark algorithm caps every channel at `0xdc`.** Apple's own `#0a84ff`,
`#ff453a` and `#ff9f0a` therefore cannot be painted at all, and the dark
palette holds the nearest reachable shades. Solve a new one by search rather
than by eye - feed candidate seeds to `theme.getDesignToken` under
`darkAlgorithm` and hill-climb toward the target - because guessing lands
several shades away and the miss is invisible until it is beside a button.

## Two constraints, and why

**Plain CSS, never utility classes.** One of the tools that shares this folder
compiles Tailwind and the other does not. A primitive styled with
`bg-surface shadow-neu` renders as an unstyled `<div>` the moment it is copied
into the app without them, so everything here is a `ui-`prefixed class in
`components.css`. An app that *does* have Tailwind keeps using it everywhere
else; it just does not reach into this folder.

**`react` and `antd` only.** No icon package (the two tools do not share one -
the six glyphs this kit needs are drawn inline in `icons.tsx`), no state
library, no router, no animation runtime. A dependency added here is a
dependency every adopting tool must also install at a compatible version, and
that is how a copyable folder turns into a package with a release process.

The kit is written for React 18 and 19 alike, and for the strictest
`tsconfig` of the two (`noUncheckedIndexedAccess` included), so it typechecks
in either repository unchanged.

## Adopting it

1. Copy the folder to `src/uikit/`.
2. Write `src/brand.ts` exporting a `BrandIdentity`.
3. In `vite.config.ts`: `import brandPlugin from "./src/uikit/vitePluginBrand"`
   and `import brand from "./src/brand"`, then add `brandPlugin(brand)` to
   `plugins`.
4. In the entry point: `import "./uikit/styles.css"` and wrap the app in
   `<ThemeProvider>`.

An app with its own settings model passes `value` and `onChange` to
`ThemeProvider` instead, and keeps owning the preference.

## Changing it

Edit it in one repository, verify there, then copy the whole folder across and
run the other repository's typecheck. Two things make that safe rather than
hopeful:

- Nothing outside the folder is edited by the copy, because nothing inside it
  names a product.
- The check is mechanical. From either repository:

  ```sh
  diff -r ../configer/frontend/src/uikit ../softwareGateway/web/src/uikit
  ```

  Empty output means the two tools are on the same design system. Anything
  else is a drift, and the fix is always to copy - never to patch one side.

**Do not "fix" a difference by adding a flag.** A prop that makes the kit
behave one way in one tool and another way in the other is the drift, written
down. If two tools genuinely need different looks, that is a preset in
`tokens.ts` - shipped to both, chosen by one line.

## Its neighbour

`tablekit/` sits beside this folder and follows exactly the same rules: copied
whole, byte-identical, `react` and `antd` only, every colour a token. It holds
the one component too big and too specific to be a primitive - the data table
with resizable, reorderable, pinnable columns - and it READS this kit's
variables without importing anything from it, which is what keeps two copyable
folders from being coupled to each other's file layout. See its own README for
which tables get it.
