- Problem 1 — Context-Aware Video Segmentation & Intelligent Ad Placement
    
    *Track: Video Understanding / AdTech*
    
    Ingest a long-form Bengali drama, segment it into semantically coherent scenes, score which scene boundaries are safe to interrupt, and match each surviving break to the most contextually appropriate brand from a synthetic catalogue — then emit a standards-compliant ad-break manifest and a playable demo.
    
    **Must get right**
    
    | **Where** | Is this timestamp a natural, non-jarring place to cut? (mid-sentence cuts are heavily penalised) |
    | --- | --- |
    | **Whether** | Is a break warranted here at all, given pacing rules (max breaks/hour, min gap, ad load %)? |
    | **What** | Which brand's creative belongs in this specific slot — dominant scene activity wins, negative_contexts are a hard block, not a soft penalty |
    
    **MVP scope**
    
    Video in → scenes segmented → break candidates scored → brand matched (must generalise to a 9th, unseen brand with zero code changes) → VMAP manifest + debug JSON + playable demo that actually cuts to the ad and resumes.
    
    **Toughest test**
    
    Held-out videos are chosen specifically to punish naive scoring: judges check for mid-dialogue cuts and blind-viewing scene quality, and any negative-context brand placed against a blocked scene (e.g. a food ad after a funeral) in the held-out set scores that criterion **zero**, not low.
    
    **Auto-disqualifiers**
    
    - Hard-coding timestamps or brand assignments for the sample videos
    - Any negative-context violation in the held-out set
    - Demo that cannot run live at presentation time
    - Substituting real company names for the synthetic brands