// Empties the folder the Go gateway embeds, but keeps .gitkeep, so a fresh
// checkout still compiles before the front end has been built.
import { mkdirSync, readdirSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const dist = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "internal", "ui", "dist");
mkdirSync(dist, { recursive: true });
for (const name of readdirSync(dist)) {
  if (name !== ".gitkeep") rmSync(join(dist, name), { recursive: true, force: true });
}
