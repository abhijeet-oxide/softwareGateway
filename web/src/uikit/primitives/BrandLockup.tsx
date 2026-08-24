import type { CSSProperties } from "react";
import type { BrandIdentity } from "../brand";

// The product's mark and name, drawn the same way in every tool.
//
// They take the identity as a PROP rather than importing it, which is the rule
// that keeps this folder copyable: a shared component that reached for
// "../brand" would be a shared component that hardcodes one product's name.

/** The mark on its brand-coloured plate: the form the product wears in the
 *  navigation, on a boot screen, and beside a sign-in card. One component, so
 *  the identity cannot differ between window sizes or states. */
export function BrandMark({
  brand,
  size = 28,
  /** draw the plate; off gives the bare glyph, for a dark surface that already
   *  provides the ground */
  tile = true,
  style,
}: {
  brand: BrandIdentity;
  size?: number;
  tile?: boolean;
  style?: CSSProperties;
}) {
  const box: CSSProperties = {
    width: size,
    height: size,
    borderRadius: tile ? Math.round(size * 0.29) : 0,
    fontSize: Math.round(size * 0.5),
    ...style,
  };
  const cls = `ui-mark${tile ? " is-tile" : ""}`;
  if (brand.logo.src) {
    return <img className={cls} src={brand.logo.src} alt={brand.appName} style={box} />;
  }
  if (brand.logo.svg) {
    return <span className={cls} style={box} dangerouslySetInnerHTML={{ __html: brand.logo.svg }} />;
  }
  return (
    <span className={cls} style={box}>
      {brand.logo.text ?? brand.appName.charAt(0)}
    </span>
  );
}

/**
 * The mark, the name, and what the product does, as one block.
 *
 * The text column is explicitly start-aligned, and that is load-bearing rather
 * than tidy. These two lines are different widths - the caption is nearly
 * always the wider one - so on any surface that centres its text (a boot card,
 * a sign-in card) the short name floats to the middle of the box the caption
 * sized, and reads as a mark with a hole punched between it and its own name.
 * A lockup must align to itself, never to whatever it was dropped into.
 */
export function BrandLockup({
  brand,
  caption = true,
  size = 28,
  tile = true,
  /** the caption text; defaults to the brand's own nav caption */
  captionText,
  /** name colour: the navigation's own foreground by default */
  onDark = false,
  style,
}: {
  brand: BrandIdentity;
  caption?: boolean;
  size?: number;
  tile?: boolean;
  captionText?: string;
  onDark?: boolean;
  style?: CSSProperties;
}) {
  return (
    <div className={`ui-lockup${onDark ? " on-dark" : ""}`} style={style}>
      <BrandMark brand={brand} size={size} tile={tile} />
      <div className="ui-lockup-text">
        <span className="ui-lockup-name">{brand.appName}</span>
        {caption && (captionText ?? brand.navCaption) && (
          <span className="ui-lockup-caption">{captionText ?? brand.navCaption}</span>
        )}
      </div>
    </div>
  );
}
