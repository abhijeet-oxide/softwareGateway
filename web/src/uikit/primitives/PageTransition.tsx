import { useEffect, useRef, type ReactNode } from "react";

/**
 * The one movement between one screen and the next.
 *
 * # Why this exists in the shared kit
 *
 * Because a change of page is the most frequent transition in either product
 * and it was the only one nobody had authored. What happened instead was
 * whatever the browser did: the old tree was thrown away, the new one mounted
 * empty, its queries resolved a beat later and the layout settled in two or
 * three steps. Nothing was slow; it simply all arrived in the wrong order, and
 * a page that assembles itself in front of somebody reads as jitter even when
 * it is fast.
 *
 * A short entrance covers exactly that window. The content arrives as one
 * object over 200ms rather than in pieces over 40, which is enough to make the
 * assembly invisible and short enough that nobody waits for it.
 *
 * # Why it is deliberately small
 *
 * 200ms and six pixels. It is not a page turn and must never feel like one:
 * anything longer, or anything that moves further, converts "fast" into
 * "animated", and an operations console is used all day by people who change
 * screens constantly. Opacity and transform only - both composited, so the
 * animation runs off the main thread and cannot itself become the stutter it
 * exists to hide.
 *
 * # The scroll, which is half the problem
 *
 * A new page inherits the previous page's scroll position, so arriving at the
 * top of a short page after leaving the bottom of a long one looked like a
 * jump. The scroller is found by walking up from this element rather than
 * being passed in, so nothing here needs to know which frame it is inside.
 */
export default function PageTransition({
  routeKey,
  children,
  /** leave the scroll where it is - for a view whose tabs are part of the key */
  keepScroll = false,
  /**
   * Fill the frame instead of flowing in it.
   *
   * A shell mounted `flush` hands its page the whole height and lets the panes
   * inside manage their own scrolling, so anything wrapped around that page has
   * to be as tall as the page or the panes collapse. An ordinary page ignores
   * this and stays a block in the normal flow.
   */
  fill = false,
}: {
  routeKey: string;
  children: ReactNode;
  keepScroll?: boolean;
  fill?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (keepScroll) return;
    const scroller = ref.current?.closest(".ui-shell-page");
    // `auto`, not `smooth`: this runs while the entrance is playing, and two
    // animations over the same pixels is the stutter, not the cure.
    scroller?.scrollTo({ top: 0, behavior: "auto" });
  }, [routeKey, keepScroll]);

  return (
    <div ref={ref} key={routeKey} className={`ui-page${fill ? " is-fill" : ""}`}>
      {children}
    </div>
  );
}
