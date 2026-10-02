/**
 * Client for Computer Use (https://computeruse.site): serverless, resumable
 * desktop containers that an agent drives with one tool, `run_js`.
 *
 * This is the Rust SDK behind UniFFI bindings; `./generated` is written by
 * `sdk/scripts/bindings.sh javascript` and is not edited by hand.
 *
 * ```ts
 * import { Client } from "computeruse";
 *
 * const client = Client.withToken(process.env.COMPUTERUSE_API_TOKEN!);
 * const session = await client.createSession({ name: "demo" });
 * const result = await session.runJs("console.log(6 * 7)");
 * console.log(result.output);
 * await session.sleep();
 * ```
 */
import generated from "./generated/computeruse";

export * from "./generated/computeruse";

// Loads the native library (prebuilds/<platform>-<arch>/) and checks that it
// is the one these bindings were generated for.
generated.initialize();
