import type { ReactNode } from "react";
import { SpinnerIcon } from "../icons";

// LoadingStage makes waiting informative: a skeleton keeps the page's shape
// while a stage line says what is actually happening ("Rendering files for
// prod-us-east...", "Computing differences...").
//
// It never fakes progress. The stage text changes only when the work does,
// which is what makes it worth reading rather than watching.
export default function LoadingStage({ stage, skeleton }: { stage: string; skeleton?: ReactNode }) {
  return (
    <div className="ui-loading">
      <div className="ui-loading-line" role="status" aria-live="polite">
        <SpinnerIcon />
        {stage}
      </div>
      <div className="ui-loading-body">{skeleton}</div>
    </div>
  );
}
