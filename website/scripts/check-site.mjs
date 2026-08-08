import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const publicDir = join(root, "public");
const html = readFileSync(join(publicDir, "index.html"), "utf8");
const headers = readFileSync(join(publicDir, "_headers"), "utf8");
const errors = [];

const requireText = (value, label) => {
  if (!html.includes(value)) errors.push(`missing ${label}: ${value}`);
};

requireText("<title>Oberth — AI-native CI · satellite Git & CI authority</title>", "page title");
requireText('rel="canonical" href="https://oberth.ci/"', "canonical URL");
requireText("Local CI at agent speed.", "primary message");
requireText("The audit chain is tamper-evident — including against the box it runs on — not tamper-proof.", "audit claim boundary");
requireText("The visibility boundary is intentional and explicit", "auth split boundary");

for (const forbidden of ["10-50x", "80% of failures", "tamper-proof audit", "one-command install is available"]) {
  if (html.toLowerCase().includes(forbidden.toLowerCase())) {
    errors.push(`unsupported marketing claim found: ${forbidden}`);
  }
}

const localAsset = /(?:href|src)="(\/[^"]+)"/g;
for (const [, rawPath] of html.matchAll(localAsset)) {
  const assetPath = rawPath.split(/[?#]/, 1)[0];
  if (assetPath === "/") continue;
  if (!existsSync(join(publicDir, assetPath))) {
    errors.push(`missing local asset: ${assetPath}`);
  }
}

const ldMatch = html.match(/<script type="application\/ld\+json">\n([\s\S]*?)\n<\/script>/);
if (!ldMatch) {
  errors.push("missing JSON-LD block");
} else {
  try {
    JSON.parse(ldMatch[1]);
  } catch (error) {
    errors.push(`invalid JSON-LD: ${error.message}`);
  }
}

// JSON-LD data blocks are non-executable, so a strict `script-src 'self'`
// needs no inline hash; every other script element must load from src.
if (!headers.includes("script-src 'self'")) {
  errors.push("CSP does not restrict script-src to 'self'");
}
if (headers.includes("unsafe-inline")) {
  errors.push("CSP allows unsafe-inline");
}
for (const [tag] of html.matchAll(/<script[^>]*>/g)) {
  if (tag.includes('type="application/ld+json"') || tag.includes('src="')) continue;
  errors.push(`inline executable script violates script-src 'self': ${tag}`);
}

for (const file of ["site.webmanifest", "sitemap.xml", "robots.txt", "404.html", "favicon.svg", "og-image.svg"]) {
  if (!existsSync(join(publicDir, file))) errors.push(`missing required file: ${file}`);
}

try {
  JSON.parse(readFileSync(join(publicDir, "site.webmanifest"), "utf8"));
} catch (error) {
  errors.push(`invalid site.webmanifest: ${error.message}`);
}

if (errors.length) {
  console.error(errors.map((error) => `- ${error}`).join("\n"));
  process.exit(1);
}

console.log("oberth.ci static site checks passed");
