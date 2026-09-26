# Demo UI redesign — deferred until the MVP pipeline works

The current embedded UI is intentionally kept in place while upload, AI analysis, and ad-break decisions are built and tested. A polished redesign is a later milestone, not a dependency for the first working demo.

## Direction to explore

- Move the presentation layer to **Next.js** if it improves the demo workflow and deployment; retain the Go API and Python AI worker.
- Use **GSAP** for restrained, purposeful transitions: analysis progress, evidence reveal, and the transition into/out of an ad break.
- Use **Three.js** only for a meaningful visual—such as a navigable story timeline or scene map. Avoid adding a 3D scene merely for decoration.
- Keep transcripts, scene evidence, confidence, and safety decisions legible and accessible; animation must not obscure evidence or imply certainty the model does not have.

## Gate before starting

Begin the redesign only after the end-to-end MVP is stable: upload a provided full-length asset, complete AI analysis, inspect timestamped evidence, generate a safe break plan, and pass the staged playback test. Preserve a usable low-motion experience and verify the final demo on the actual judging hardware/network.

## Current boundary

No Next.js, Three.js, or GSAP dependencies are introduced during the AI evidence phase. The existing UI remains the demo surface until the underlying product flow is validated.
