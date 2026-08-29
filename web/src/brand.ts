import type { BrandIdentity } from "./uikit";

// WHO THIS DEPLOYMENT IS. The one file that says "Software Gateway".
//
// Everything else visual - the palette, the spacing, the controls, the
// primitives - comes from `uikit/`, which is byte-identical to the copy in
// every other tool on this platform. This file is the deliberate exception:
// two products that look the same must still say which one you are looking at.
//
// So a rebrand is this file plus `uikit/tokens.ts`, and nothing else. Nothing
// in the application names a colour or a product; a page that reached for its
// own hex would be one that a rebrand silently misses.

const brand: BrandIdentity = {
  appName: "Software Gateway",
  tagline: "Software Lifecycle Management",
  navCaption: "SOFTWARE LIFECYCLE",
  // A PACKAGE: a closed carton seen from a three-quarter angle, its lid seam
  // drawn across the top and its two faces separated by tone.
  //
  // The sibling tool's mark is a stack of configuration layers; this one is
  // built the same way - one solid form, symmetrical about the vertical axis,
  // rounded joins, no outline - so the two read as a family rather than as two
  // unrelated logos. What they DRAW is the difference, and it is the right
  // difference: that tool arranges layers, this one moves packages.
  logo: {
    svg:
      "<svg width='19' height='19' viewBox='0 0 24 24' fill='none' " +
      "stroke-linejoin='round' stroke-linecap='round' xmlns='http://www.w3.org/2000/svg'>" +
      // the two side faces, the right one held back a shade so the box turns
      "<path d='M12 12.1 L21 7.4 L21 16.6 L12 21.3 Z' fill='white' fill-opacity='0.55'/>" +
      "<path d='M12 12.1 L3 7.4 L3 16.6 L12 21.3 Z' fill='white' fill-opacity='0.78'/>" +
      // the lid
      "<path d='M12 2.7 L21 7.4 L12 12.1 L3 7.4 Z' fill='white'/>" +
      // the seam down the middle of the lid, which is what makes it read as a
      // package rather than a plain cube
      "<path d='M7.5 5.05 L16.5 9.75' stroke='white' stroke-opacity='0.45' stroke-width='1.5'/></svg>",
  },
  // The same package, at favicon scale: fewer parts, because at 16px the seam
  // and the two face tones are all that survive.
  favicon:
    "<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'>" +
    "<rect width='32' height='32' rx='8' fill='#0071e3'/>" +
    "<g stroke-linejoin='round' stroke-linecap='round'>" +
    "<path d='M16 16.2 L27 10.4 L27 21.6 L16 27.4 Z' fill='white' fill-opacity='0.55'/>" +
    "<path d='M16 16.2 L5 10.4 L5 21.6 L16 27.4 Z' fill='white' fill-opacity='0.78'/>" +
    "<path d='M16 4.6 L27 10.4 L16 16.2 L5 10.4 Z' fill='white'/>" +
    "<path d='M10.5 7.5 L21.5 13.3' stroke='white' stroke-opacity='0.45' stroke-width='1.8'/>" +
    "</g></svg>",
};

export default brand;
