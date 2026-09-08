# Sign-in screen branding

The seeder uploads `logo.svg` and `logo-dark.svg` to ZITADEL's label policy, so
the screen a person meets before they are anybody carries this product's mark
rather than ZITADEL's.

Replace them in place, or point elsewhere without touching the repository:

```
BRANDING_LOGO_FILE=/branding/my-logo.svg
BRANDING_LOGO_DARK_FILE=/branding/my-logo-dark.svg
BRANDING_PRIMARY_COLOR=#0b7285
BRANDING_PRIMARY_COLOR_DARK=#22b8cf
```

The paths are inside the `zitadel-init` container; mount your own directory
over `/branding` to use files from outside the repository. A missing file is
not an error - ZITADEL keeps whatever it has.

SVG, PNG and JPEG are accepted. The light logo is drawn on a white surface and
the dark one on a near-black surface, so a single-colour mark needs both.

Nothing here is visible until the label policy is ACTIVATED, which the seeder
does after uploading. That step is easy to leave out by hand: every write
succeeds, the console shows the new colours, and the sign-in screen keeps the
old ones.
