import type { CSSProperties } from "react";
import type { BrandIdentity } from "../brand";

// The product's mark, drawn the same way in both tools.
//
// It takes the identity as a PROP rather than importing it, which is the rule
// that keeps this folder copyable: a shared component that reached for
// "../brand" would be a shared component that hardcodes one product's name.
export function BrandMark({
  brand,
  size = 19,
  style,
}: {
  brand: BrandIdentity;
  size?: number;
  style?: CSSProperties;
}) {
  const box: CSSProperties = {
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    width: size,
    height: size,
    flexShrink: 0,
    ...style,
  };
  if (brand.logo.src) {
    return <img src={brand.logo.src} alt={brand.appName} style={box} />;
  }
  if (brand.logo.svg) {
    return <span style={box} dangerouslySetInnerHTML={{ __html: brand.logo.svg }} />;
  }
  return (
    <span style={{ ...box, fontWeight: 700, fontSize: size * 0.6 }}>
      {brand.logo.text ?? brand.appName.charAt(0)}
    </span>
  );
}

/** The mark, the name and the caption under it: the block at the top of the
 *  navigation, identical in every tool that mounts it. */
export function BrandLockup({
  brand,
  caption = true,
  style,
}: {
  brand: BrandIdentity;
  caption?: boolean;
  style?: CSSProperties;
}) {
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 10, minWidth: 0, ...style }}>
      <BrandMark brand={brand} />
      <div style={{ minWidth: 0, lineHeight: 1.2 }}>
        <div
          style={{
            fontWeight: 700,
            fontSize: 15,
            color: "var(--nav-fg-active)",
            overflow: "hidden",
            textOverflow: "ellipsis",
            whiteSpace: "nowrap",
          }}
        >
          {brand.appName}
        </div>
        {caption && brand.navCaption && (
          <div style={{ fontSize: 10, color: "var(--nav-fg)", letterSpacing: 0.4 }}>
            {brand.navCaption}
          </div>
        )}
      </div>
    </div>
  );
}
