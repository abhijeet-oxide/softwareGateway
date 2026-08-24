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
  // A package moving through a gate: a solid carton over two receding echoes,
  // the same construction as the sibling tool's layered mark, so the two read
  // as one family without being the same drawing. Symmetrical about the
  // vertical axis; rounded joins keep it soft rather than spiky.
  logo: {
    svg:
      "<svg width='19' height='19' viewBox='0 0 24 24' fill='none' " +
      "stroke-linejoin='round' stroke-linecap='round' xmlns='http://www.w3.org/2000/svg'>" +
      "<path d='M3.6 15.4 L12 19.9 L20.4 15.4' stroke='white' stroke-width='1.9' stroke-opacity='0.32'/>" +
      "<path d='M3.6 11.7 L12 16.2 L20.4 11.7' stroke='white' stroke-width='1.9' stroke-opacity='0.58'/>" +
      "<path d='M12 3.5 L20.6 7.9 L20.6 12.3 L12 16.7 L3.4 12.3 L3.4 7.9 Z' fill='white'/>" +
      "<path d='M12 3.5 L20.6 7.9 L12 12.3 L3.4 7.9 Z' fill='white' fill-opacity='0.55'/></svg>",
  },
  favicon:
    "<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'>" +
    "<rect width='32' height='32' rx='8' fill='#0057b8'/>" +
    "<g fill='none' stroke='white' stroke-linejoin='round' stroke-linecap='round'>" +
    "<path d='M8 20.5 L16 24.7 L24 20.5' stroke-width='2.4' stroke-opacity='0.32'/>" +
    "<path d='M8 17 L16 21.2 L24 17' stroke-width='2.4' stroke-opacity='0.58'/>" +
    "<path d='M16 6.3 L24 10.4 L24 14.6 L16 18.7 L8 14.6 L8 10.4 Z' fill='white' stroke='none'/></g></svg>",
};

export default brand;
