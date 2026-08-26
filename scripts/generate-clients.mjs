#!/usr/bin/env node

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const specPath = resolve(root, "openapi/cc-search.json");
const spec = JSON.parse(readFileSync(specPath, "utf8"));

if (spec.openapi !== "3.1.0") throw new Error(`expected OpenAPI 3.1.0, got ${spec.openapi}`);
const requiredPaths = ["/openapi.json", "/v1/health", "/v1/search", "/v1/last", "/v1/read", "/v1/sessions", "/v1/activities", "/v1/activity", "/v1/context", "/v1/commands", "/v1/info", "/v1/doctor", "/v1/rebuild"];
for (const path of requiredPaths) {
  if (!spec.paths?.[path]) throw new Error(`spec is missing ${path}`);
}

function copyTemplate(template, destination, comment) {
  mkdirSync(dirname(destination), { recursive: true });
  const contents = readFileSync(resolve(root, template), "utf8");
  writeFileSync(destination, `${comment} Generated from openapi/cc-search.json by scripts/generate-clients.mjs.\n${contents}`);
}

copyTemplate("scripts/templates/cc_search_client.py", "clients/python/cc_search_client/__init__.py", "#");
copyTemplate("scripts/templates/index.ts", "clients/typescript/src/index.ts", "//");

mkdirSync(resolve(root, "clients/python"), { recursive: true });
writeFileSync(resolve(root, "clients/python/pyproject.toml"), `[project]\nname = "cc-search-client"\nversion = "0.1.0"\ndescription = "Agent client for the local cc-search OpenAPI service"\nrequires-python = ">=3.10"\ndependencies = []\n\n[tool.setuptools.packages.find]\nwhere = ["."]\n`);

mkdirSync(resolve(root, "clients/typescript"), { recursive: true });
writeFileSync(resolve(root, "clients/typescript/package.json"), `${JSON.stringify({
  name: "@andrewmuldowney/cc-search-client",
  version: "0.1.0",
  private: true,
  type: "module",
  exports: { ".": "./src/index.ts" },
  scripts: { test: "node --experimental-strip-types --test test/client.test.ts" },
}, null, 2)}\n`);

console.log(`generated clients from ${specPath}`);
