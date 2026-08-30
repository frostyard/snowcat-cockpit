#!/usr/bin/env node
// Zero-dependency docs-integrity gate: index coverage, relative links,
// repo-contained symlinks, and pinned acceptance-metric headings (ADR-0007).
import { readFileSync, readdirSync, lstatSync, existsSync, realpathSync } from "node:fs";
import { join, dirname, resolve, relative, sep } from "node:path";

const root = resolve(dirname(new URL(import.meta.url).pathname), "..");
const thresholds = JSON.parse(readFileSync(join(root, ".coverage-thresholds.json"), "utf8"));

if (thresholds.never_relax !== true) {
  console.error("FATAL: .coverage-thresholds.json never_relax must be true");
  process.exit(1);
}

const failures = [];
const rate = (pass, total) => (total === 0 ? 1 : pass / total);
const categories = ["adr", "design", "specs", "plans"];

// Every canonical category doc appears in the index; templates are contracts,
// not index entries.
const indexText = readFileSync(join(root, "docs/README.md"), "utf8");
let docsTotal = 0;
let docsIndexed = 0;
for (const category of categories) {
  for (const name of readdirSync(join(root, "docs", category))) {
    if (!name.endsWith(".md") || name === "TEMPLATE.md") continue;
    docsTotal++;
    if (indexText.includes(`${category}/${name}`)) docsIndexed++;
    else failures.push(`index: docs/${category}/${name} has no line in docs/README.md`);
  }
}

// Relative Markdown links in canonical repository docs must resolve.
const mdFiles = [
  join(root, "AGENTS.md"),
  join(root, "README.md"),
  join(root, "docs/README.md"),
  join(root, "docs/org-adrs.md"),
];
for (const category of categories) {
  for (const name of readdirSync(join(root, "docs", category))) {
    if (name.endsWith(".md") && name !== "TEMPLATE.md") mdFiles.push(join(root, "docs", category, name));
  }
}
const skillsRoot = join(root, ".agents", "skills");
(function collectSkillDocs(directory) {
  for (const name of readdirSync(directory)) {
    const path = join(directory, name);
    const stat = lstatSync(path);
    if (stat.isDirectory()) collectSkillDocs(path);
    else if (name.endsWith(".md")) mdFiles.push(path);
  }
})(skillsRoot);

let linksTotal = 0;
let linksOK = 0;
for (const file of mdFiles) {
  const text = readFileSync(file, "utf8").replace(/```[\s\S]*?```/g, "");
  const isSkillDoc = file.startsWith(skillsRoot + sep);
  for (const match of text.matchAll(/\[[^\]]*\]\(([^)\s]+)\)/g)) {
    const target = match[1];
    if (/^[a-z][a-z+.-]*:/i.test(target) || target.startsWith("#") || (isSkillDoc && target.startsWith("/"))) continue;
    linksTotal++;
    const path = resolve(dirname(file), target.split("#")[0]);
    if (isSkillDoc && path !== skillsRoot && !path.startsWith(skillsRoot + sep)) {
      failures.push(`link: ${relative(root, file)} -> ${target} escapes .agents/skills`);
    } else if (existsSync(path)) linksOK++;
    else failures.push(`link: ${relative(root, file)} -> ${target} does not resolve`);
  }
}

// Every symlink resolves within this repository. Build/test output trees are
// not repository surfaces.
const skipDirectories = new Set([".git", "build", "completions", "dist", "manpages", "node_modules"]);
const symlinks = [];
(function walk(directory) {
  for (const name of readdirSync(directory)) {
    const path = join(directory, name);
    const stat = lstatSync(path);
    if (stat.isSymbolicLink()) symlinks.push(path);
    else if (stat.isDirectory() && !skipDirectories.has(name)) walk(path);
  }
})(root);
let linksResolved = 0;
for (const path of symlinks) {
  try {
    const real = realpathSync(path);
    if (real === root || real.startsWith(root + sep)) linksResolved++;
    else failures.push(`symlink: ${relative(root, path)} escapes the repo (${real})`);
  } catch {
    failures.push(`symlink: ${relative(root, path)} is broken`);
  }
}

// The README's non-mutating "inspect this machine" quick-start block must
// never present install-kit as an inspection command: it materializes the
// locked worker kit on disk and is documented separately, right after that
// block.
const readmeText = readFileSync(join(root, "README.md"), "utf8");
const inspectionIntro = "creating state or claiming work:";
const introIndex = readmeText.indexOf(inspectionIntro);
if (introIndex === -1) {
  failures.push("readme: quick-start no-state-creation intro sentence is missing");
} else {
  const fenceStart = readmeText.indexOf("```bash", introIndex);
  const fenceEnd = readmeText.indexOf("```", fenceStart + "```bash".length);
  if (fenceStart === -1 || fenceEnd === -1) {
    failures.push("readme: quick-start no-state-creation code block is missing");
  } else {
    const inspectionBlock = readmeText.slice(fenceStart, fenceEnd);
    if (inspectionBlock.includes("install-kit")) {
      failures.push("readme: install-kit is presented as a non-mutating inspection command");
    }
  }
}
if (!readmeText.includes("`--skills-dir` (default\n`$HOME/.agents/skills`)")) {
  failures.push("readme: install-kit's materialized default --skills-dir is not documented");
}

const metric = readFileSync(join(root, "docs/specs/pr-acceptance-metric.md"), "utf8");
for (const heading of ["## Definition", "## Rules"]) {
  if (!metric.split("\n").includes(heading)) failures.push(`pin: PR acceptance metric is missing ${heading}`);
}

const results = {
  docs_index_coverage: rate(docsIndexed, docsTotal),
  link_integrity: rate(linksOK, linksTotal),
  symlink_resolution: rate(linksResolved, symlinks.length),
};
for (const failure of failures) console.error(`FAIL ${failure}`);
let ok = failures.length === 0;
for (const [key, value] of Object.entries(results)) {
  const required = thresholds[key];
  const met = value >= required;
  if (!met) ok = false;
  console.log(`${met ? "ok  " : "FAIL"} ${key}: ${value.toFixed(3)} (required ${required})`);
}
console.log(`checked: ${docsTotal} docs, ${linksTotal} links, ${symlinks.length} symlinks`);
process.exit(ok ? 0 : 1);
