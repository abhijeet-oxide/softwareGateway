# `tablekit/` - the shared data table

**This folder is byte-identical in every tool that uses it**, exactly like its
neighbour `uikit/`. Copy it whole, in either direction, and both products keep
behaving the same way.

| repository | path |
| --- | --- |
| `configer` | `frontend/src/tablekit/` |
| `softwareGateway` | `web/src/tablekit/` |

```sh
diff -r ../configer/frontend/src/tablekit ../softwareGateway/web/src/tablekit
```

Empty output means the two tools are on the same table. Anything else is a
drift, and the fix is always to copy - never to patch one side.

## What it is

`Table` is a drop-in replacement for Ant Design's `Table`. Every prop the
component library takes still works; on top of them it adds the column
behaviour a real estate needs and that neither product should be writing twice:

| feature | how a person reaches it |
| --- | --- |
| Resize a column | drag the seam between two headers, or focus the handle and use the arrow keys |
| Reorder columns | drag a header's grab handle |
| Pin a column left or right | the header's own menu (right-click, or the caret) |
| Show and hide columns | the eye in the toolbar, with a search over the names |
| Autofit one column, or all of them | the header menu, and the same menu's "fit table" |
| Export what is on screen | the toolbar, as CSV, Excel or JSON |
| Reset width, order, or everything | the header menu |

When a table is rendered inside a framed surface such as an Ant Design `Card`,
pass `toolbarPlacement="outside"` to keep the export and column controls above
the surface without reserving a blank toolbar row inside it. The parent layout
must provide roughly 36px of vertical space above the table for those controls.
The default placement is `inside`.

Preferences persist per table, keyed by `tableEnhancedKey`, in the browser's
storage - so a person's own layout survives a reload without the server ever
being told about it.

It is the upstream `antd-table-enhanced` package's source, vendored rather than
installed, for the same reason `uikit/` is vendored: a copied folder has no
version skew, no install step, and no registry to be unreachable from an
air-gapped build.

## The two rules

> **Nothing in this folder names a product**, and nothing in it imports one.

The only dependencies are `react` and `antd` - the same pair `uikit/` allows,
and for the same reason. In particular **no icon package**: the two tools do not
share one (one keeps a Phosphor-backed registry, the other compiles Iconify sets
at build time), so the eleven glyphs this component needs are drawn inline in
`icons.tsx`. A shared component that imports from either app's icon library
stops being copyable the moment it lands in the other repository.

> **Every colour is a token.**

`Table.module.css` names `var(--brand)`, `var(--surface-2)`, `var(--border)` and
the rest from the shared design system, never a hex. That is the one substantive
difference from the upstream package, and it is not cosmetic: upstream ships
hardcoded blues and greys, which is right for a package that must work with no
design system underneath it and wrong here - a hardcoded `#fafafa` header is a
white band across a dark page. Because the whole file is tokens, a table follows
the light/dark switch, the density setting and the active preset without this
component knowing that any of them exist.

**This folder therefore depends on `uikit/` being mounted**, which is the one
thing to check when adopting it: the variables come from `uikit/tokens.css` and
from the colour layer `vitePluginBrand.ts` inlines. It does not IMPORT uikit -
that would couple two copyable folders to each other's file layout - it just
reads the variables uikit puts on `:root`.

## Adopting it

1. Copy the folder to `src/tablekit/`, beside `src/uikit/`.
2. Import `Table` from it instead of from `antd` wherever a table is worth
   rearranging.

Nothing else. No stylesheet to register (the CSS module travels with the
component), no provider to mount, no build configuration.

## Which tables get it

Not all of them, and the distinction is worth keeping. This `Table` is for a
**working surface**: many columns, rows a person scans and compares, a layout
worth keeping between visits. A three-column summary inside a card, a two-row
key/value list, or a table nested inside an expanded row is not that, and giving
it a toolbar and a drag handle spends chrome on a reader who has nothing to
rearrange.

The rule of thumb both products follow: **five or more columns, or a table that
is the point of its page.** Everything else stays on the component library's
plain `Table`.

## Changing it

Edit it in one repository, verify there, then copy the whole folder across and
run the other repository's typecheck. The component is written for React 18 and
19 alike and for the strictest `tsconfig` of the two, so it typechecks in either
repository unchanged.

Upstream is `antd-table-enhanced`. A fix that belongs to the component rather
than to this platform should go there first and be copied down, so the vendored
copy stays a copy.
