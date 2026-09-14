# software/schema - what an instance values file is allowed to say

`values.schema.json` is a copy of the chart's own schema, staged by
`task chart:stage` so the two cannot drift. Helm checks it before every render,
so a misspelled key is a failed reconciliation with the key named, rather than a
setting that silently did not apply.

Point an editor at it to get completion and validation while writing
`instances/<instance>/values/values.yaml`:

```yaml
# yaml-language-server: $schema=../../../software/schema/values.schema.json
```
