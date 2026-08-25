#!/usr/bin/env node
// Fails when a CSS rule was deleted while the source still uses it.
//
// WHY THIS EXISTS. Extracting the shared design system meant deleting blocks
// from each app's stylesheet as the kit took them over, and the deletions were
// made by "from this selector down to that one". Stylesheets are not sorted by
// topic: between the boot screen and the sign-in screen sat the New application
// wizard and every responsive dialog rule, and one such cut took all of them
// with it. Nothing failed. TypeScript cannot see a class name, the build
// happily emits CSS for rules nobody wrote, and the app renders - just wrong.
// It surfaced as two cards that had been side by side stacking on top of each
// other, days later, in a photograph.
//
// So this is the check that closes that loop: compare the stylesheets against a
// base revision, and for every class that has DISAPPEARED, ask whether the
// source still references it. That is exact - no heuristics about which class
// names are "real" - and it is precisely the mistake it is meant to catch.
//
//   node src/uikit/check-styles.mjs [baseRef]     (default: origin/main, then main)
//
// It only reports classes that vanished from CSS and are still used. Moving a
// rule between stylesheets is fine; so is deleting one nothing references.

import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, resolve } from "node:path";

const SRC = resolve(process.argv[2] ?? new URL("..", import.meta.url).pathname);
const BASE_ARG = process.argv[3];

function git(args, opts = {}) {
  return execFileSync("git", args, { encoding: "utf8", cwd: SRC, ...opts });
}

function pickBase() {
  if (BASE_ARG) return BASE_ARG;
  for (const ref of ["origin/main", "main", "origin/master", "master"]) {
    try {
      git(["rev-parse", "--verify", "--quiet", ref], { stdio: ["pipe", "pipe", "ignore"] });
      return ref;
    } catch {
      // try the next one; a shallow or freshly initialized clone may have none
    }
  }
  return null;
}

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    if (name === "node_modules" || name === "dist" || name.startsWith(".")) continue;
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else out.push(p);
  }
  return out;
}

/** Every class name a stylesheet DEFINES. */
function classesIn(css) {
  const out = new Set();
  const body = css.replace(/\/\*[\s\S]*?\*\//g, "");
  for (const m of body.matchAll(/([^{}]+)\{/g)) {
    const head = m[1].trim();
    if (head.startsWith("@")) continue;
    for (const cls of head.matchAll(/\.(-?[_a-zA-Z][\w-]*)/g)) out.add(cls[1]);
  }
  return out;
}

const base = pickBase();
if (!base) {
  console.log("check-styles: no base revision to compare against, skipping.");
  process.exit(0);
}

const files = walk(SRC);
const cssFiles = files.filter((f) => f.endsWith(".css"));
const srcFiles = files.filter((f) => /\.(tsx?|jsx?|html)$/.test(f));

// what the stylesheets define now
const now = new Set();
for (const f of cssFiles) for (const c of classesIn(readFileSync(f, "utf8"))) now.add(c);

// what they defined at the base revision
const before = new Set();
let baseFiles;
try {
  baseFiles = git(["ls-tree", "-r", "--name-only", base, "--", "."]).split("\n").filter(Boolean);
} catch {
  console.log(`check-styles: cannot read ${base}, skipping.`);
  process.exit(0);
}
const repoRoot = git(["rev-parse", "--show-toplevel"]).trim();
for (const rel of baseFiles.filter((f) => f.endsWith(".css"))) {
  let text;
  try {
    // "./" so the path is read relative to this directory: git ls-tree prints
    // cwd-relative names, and git show would otherwise look for them at the
    // repository root and quietly find nothing.
    text = git(["show", `${base}:./${rel}`]);
  } catch {
    // A stylesheet that did not exist at the base revision has nothing to
    // have lost.
    continue;
  }
  for (const c of classesIn(text)) before.add(c);
}

const gone = [...before].filter((c) => !now.has(c));
if (gone.length === 0) {
  console.log("check-styles: no CSS class was removed.");
  process.exit(0);
}

// Which of them the source still uses.
//
// Only CLASS-NAME POSITIONS count: the tokens inside string and template
// literals, with comments stripped first. Searching the raw text for the name
// instead reports every rule whose class happens to be an ordinary English
// word - "rail" appears in a dozen sentences about the navigation rail - and a
// check that cries wolf is a check people start passing with --no-verify.
function classTokens(text) {
  const code = text
    .replace(/\/\*[\s\S]*?\*\//g, " ")
    .replace(/(^|[^:])\/\/[^\n]*/g, "$1 ");
  const tokens = new Set();
  for (const m of code.matchAll(/"([^"\n]*)"|'([^'\n]*)'|`([^`]*)`/g)) {
    const literal = m[1] ?? m[2] ?? m[3] ?? "";
    for (const t of literal.split(/[\s]+/)) if (t) tokens.add(t);
  }
  return tokens;
}

const source = srcFiles.map((f) => [relative(repoRoot, f), classTokens(readFileSync(f, "utf8"))]);
const orphans = [];
for (const c of gone) {
  const where = source.filter(([, toks]) => toks.has(c)).map(([f]) => f);
  if (where.length) orphans.push({ cls: c, where });
}

if (orphans.length === 0) {
  console.log(`check-styles: ${gone.length} class(es) removed, none still referenced.`);
  process.exit(0);
}

console.error(
  `\ncheck-styles: ${orphans.length} class(es) were deleted from CSS but the source still uses them.\n` +
    `A rule was probably removed by the distance between two markers rather than by rule.\n`,
);
for (const { cls, where } of orphans) {
  console.error(`  .${cls}`);
  for (const f of where.slice(0, 4)) console.error(`      used in ${f}`);
}
console.error("");
process.exit(1);
