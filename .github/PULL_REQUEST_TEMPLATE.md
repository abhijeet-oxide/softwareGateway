## What changed

<!-- One or two sentences. What a reader needs to know before looking at the diff. -->

## Why

<!-- The problem, and why this is the fix rather than the alternative. -->

## How it was checked

<!-- `task check`, plus whatever else applies: a compose run, a rendered chart,
     a cluster. Say what you actually did. -->

- [ ] `task check`
- [ ] deployment changed → `task chart:stage && task flux:build && task chart:template`
- [ ] configuration changed (`config/`) → says what it costs at rollout: nothing, one Job, or a rolling update
