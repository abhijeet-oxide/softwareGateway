import type { ReactNode } from "react";

// Kbd is the small keycap used for keyboard-shortcut hints, styled once so
// every shortcut reads the same wherever it is shown.
export default function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="ui-kbd">{children}</kbd>;
}
