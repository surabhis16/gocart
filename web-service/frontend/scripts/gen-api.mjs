// Regenerates src/api/generated/*.d.ts from the services' OpenAPI documents in
// docs/openapi. The TypeScript types are therefore derived from the same files
// the Go services are tested against: if a service changes its contract, the
// front end stops compiling until it catches up.
import { execFileSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const specs = join(here, "..", "..", "..", "docs", "openapi");
const out = join(here, "..", "src", "api", "generated");
const cli = join(here, "..", "node_modules", "openapi-typescript", "bin", "cli.js");

mkdirSync(out, { recursive: true });
for (const name of ["product", "order", "cart", "payment"]) {
  execFileSync(process.execPath, [cli, join(specs, `${name}.yaml`), "-o", join(out, `${name}.d.ts`)], {
    stdio: "inherit",
  });
}
