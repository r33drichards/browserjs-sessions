import { defineConfig } from "tsup";

// One ES module and one declaration file. The generated sources import each
// other without file extensions, which Node's ES module loader does not
// resolve, so they are bundled; the UniFFI runtime stays a dependency.
export default defineConfig({
  entry: ["src/index.ts"],
  format: ["esm"],
  target: "node20",
  dts: true,
  clean: true,
  sourcemap: false,
  external: ["@ubjs/core", "@ubjs/node"],
});
