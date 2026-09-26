"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import gsap from "gsap";
import * as THREE from "three";

const time = (seconds) => {
  const total = Math.max(0, Math.floor(Number(seconds) || 0));
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, "0")}`;
};

function SignalOrb() {
  const holder = useRef(null);
  const [fallback, setFallback] = useState(false);

  useEffect(() => {
    const element = holder.current;
    if (!element || window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
    let renderer;
    let frame;
    let resize;
    try {
      const scene = new THREE.Scene();
      const camera = new THREE.PerspectiveCamera(40, 1, 0.1, 100);
      camera.position.z = 4.6;
      renderer = new THREE.WebGLRenderer({ alpha: true, antialias: true, powerPreference: "low-power" });
      renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 1.5));
      element.appendChild(renderer.domElement);
      const geometry = new THREE.IcosahedronGeometry(1.55, 3);
      const mesh = new THREE.Mesh(geometry, new THREE.MeshBasicMaterial({ color: 0xd9b4ff, wireframe: true, transparent: true, opacity: 0.4 }));
      const inner = new THREE.Mesh(new THREE.IcosahedronGeometry(1.36, 2), new THREE.MeshBasicMaterial({ color: 0x432c66, transparent: true, opacity: 0.42 }));
      scene.add(mesh, inner);
      const points = [];
      for (let i = 0; i < 130; i++) {
        const a = i * 2.399963;
        const z = 1 - (i / 129) * 2;
        const radius = Math.sqrt(1 - z * z) * 1.67;
        points.push(radius * Math.cos(a), radius * Math.sin(a), z * 1.67);
      }
      const dots = new THREE.BufferGeometry();
      dots.setAttribute("position", new THREE.Float32BufferAttribute(points, 3));
      scene.add(new THREE.Points(dots, new THREE.PointsMaterial({ color: 0xffcba3, size: 0.025, transparent: true, opacity: 0.9 })));
      resize = () => {
        const width = element.clientWidth;
        const height = element.clientHeight;
        if (!width || !height) return;
        camera.aspect = width / height;
        camera.updateProjectionMatrix();
        renderer.setSize(width, height, false);
      };
      window.addEventListener("resize", resize);
      resize();
      const animate = () => {
        mesh.rotation.y += 0.0018;
        mesh.rotation.x += 0.0007;
        inner.rotation.y -= 0.0012;
        renderer.render(scene, camera);
        frame = window.requestAnimationFrame(animate);
      };
      animate();
      return () => {
        window.cancelAnimationFrame(frame);
        window.removeEventListener("resize", resize);
        geometry.dispose();
        inner.geometry.dispose();
        dots.dispose();
        mesh.material.dispose();
        inner.material.dispose();
        renderer.dispose();
        renderer.domElement.remove();
      };
    } catch {
      renderer?.dispose();
      setFallback(true);
    }
  }, []);

  return <div ref={holder} className={`signal-orb${fallback ? " signal-orb-fallback" : ""}`} aria-label="Animated abstract scene-intelligence signal" role="img"><span className="orb-core" /></div>;
}

function LineChart({ pacing }) {
  const values = pacing?.tension || [];
  const points = values.map((value, index) => `${index / Math.max(1, values.length - 1) * 100},${98 - Math.max(0, Math.min(1, value)) * 88}`).join(" ");
  if (values.length < 2) return <div className="chart-empty">The emotional map appears after scene analysis.</div>;
  return <svg className="mood-chart" viewBox="0 0 100 100" preserveAspectRatio="none" role="img" aria-label="AI-estimated emotional intensity across the programme"><defs><linearGradient id="mood-stroke" x1="0" x2="1"><stop stopColor="#b990ff" /><stop offset="1" stopColor="#ffa68e" /></linearGradient></defs><polyline points={points} fill="none" stroke="url(#mood-stroke)" strokeWidth="2.1" vectorEffect="non-scaling-stroke" strokeLinecap="round" strokeLinejoin="round" /></svg>;
}

function Funnel({ transcript, review }) {
  const shots = transcript?.shot_boundaries?.length || 0;
  const potential = (transcript?.break_candidates || []).filter((item) => item.potential).length;
  const selected = review?.selected?.length ?? (transcript?.break_candidates || []).filter((item) => item.decision === "accepted").length;
  return <div className="funnel-grid"><div><span>01 / DETECT</span><strong>{shots}</strong><small>shot boundaries</small></div><div><span>02 / UNDERSTAND</span><strong>{potential}</strong><small>safe potential moments</small></div><div><span>03 / DECIDE</span><strong>{selected}</strong><small>reviewed ad breaks</small></div></div>;
}

function Timeline({ transcript, duration, review }) {
  const shots = (transcript?.shot_boundaries || []).slice(0, 260);
  const candidates = transcript?.break_candidates || [];
  const selected = review?.selected?.length ? review.selected : candidates.filter((item) => item.decision === "accepted");
  return <div className="timeline-wrap"><div className="timeline-title"><span>STORY SIGNALS</span><span>{time(duration)} total runtime</span></div><div className="signal-timeline" aria-label="Shot boundaries, safe scene changes, and reviewed ad breaks"><div className="timeline-track" />{shots.map((at, index) => <i key={`shot-${index}`} className="timeline-cut" style={{ left: `${at / duration * 100}%` }} />)}{candidates.filter((item) => item.potential).map((item, index) => <i key={`potential-${index}`} className="timeline-potential" title={`Potential at ${time(item.time)}`} style={{ left: `${item.time / duration * 100}%` }} />)}{selected.map((item, index) => <a key={`selected-${index}`} className="timeline-selected" href={`/?job=${encodeURIComponent(review?.jobId || "")}`} title={`Ad break at ${time(item.time)} — open studio to review`} style={{ left: `${item.time / duration * 100}%` }} aria-label={`Reviewed ad break at ${time(item.time)}`} />)}</div><div className="timeline-labels"><span>START</span><span>SHOT CUT</span><span>SAFE MOMENT</span><span>AD BREAK</span><span>END</span></div></div>;
}

export default function Page() {
  const [jobs, setJobs] = useState([]);
  const [job, setJob] = useState(null);
  const [selectedId, setSelectedId] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const hero = useRef(null);

  useEffect(() => {
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
    const animation = gsap.fromTo(hero.current?.querySelectorAll("[data-reveal]") || [], { y: 18 }, { y: 0, duration: 0.7, stagger: 0.11, ease: "power2.out" });
    return () => animation.kill();
  }, []);

  useEffect(() => {
    let current = true;
    fetch("/api/jobs", { cache: "no-store" }).then(async (response) => {
      if (!response.ok) throw new Error(`Job list returned ${response.status}`);
      return response.json();
    }).then((data) => {
      if (!current) return;
      const list = Array.isArray(data.jobs) ? [...data.jobs].sort((a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt)) : [];
      setJobs(list);
      setSelectedId((id) => id || list.find((item) => item.status === "completed")?.id || list[0]?.id || "");
      setError("");
    }).catch((failure) => { if (current) setError(failure.message || "Jobs could not be loaded."); }).finally(() => { if (current) setLoading(false); });
    return () => { current = false; };
  }, []);

  useEffect(() => {
    if (!selectedId) { setJob(null); return; }
    let current = true;
    fetch(`/api/jobs/${encodeURIComponent(selectedId)}`, { cache: "no-store" }).then(async (response) => {
      if (!response.ok) throw new Error(`Job returned ${response.status}`);
      return response.json();
    }).then((data) => { if (current) { setJob(data); setError(""); } }).catch((failure) => { if (current) setError(failure.message || "Analysis could not be loaded."); });
    return () => { current = false; };
  }, [selectedId]);

  const transcript = job?.transcript;
  const duration = Number(job?.media?.durationSeconds || transcript?.duration || 0);
  const scenes = transcript?.scenes || [];
  const safeCandidates = useMemo(() => (transcript?.break_candidates || []).filter((item) => item.potential), [transcript]);
  const studioLink = job ? `/?job=${encodeURIComponent(job.id)}` : "/";

  return <main className="site-shell">
    <header className="top-nav"><a className="wordmark" href="/demo/" aria-label="SceneSense demo home"><span className="brand-glyph">✳</span>scene<span>sense</span><i>LAB</i></a><nav><a href="#intelligence">INTELLIGENCE</a><a href="#story-map">STORY MAP</a><a className="nav-cta" href={studioLink}>OPEN STUDIO <span>↗</span></a></nav></header>
    <section className="hero" ref={hero}><div className="hero-copy"><div className="eyebrow" data-reveal><span className="live-dot" /> HOICHOI HACKATHON · AI AD INTELLIGENCE</div><h1 data-reveal>Find the <em>right moment.</em><br />Not just a gap.</h1><p data-reveal>SceneSense reads the story behind a Bengali film—its cuts, voices, emotions, and cultural context—then recommends ad moments worth a viewer’s attention.</p><div className="hero-actions" data-reveal><a className="button-primary" href={studioLink}>Explore the working studio <span>↗</span></a><a className="button-ghost" href="#intelligence">See the intelligence <span>↓</span></a></div><div className="hero-proof" data-reveal><span><i>01</i> AI SCENE REASONING</span><span><i>02</i> VIEWER-FIRST POLICY</span><span><i>03</i> HUMAN CONTROL</span></div></div><div className="hero-visual" data-reveal><div className="orb-caption orb-caption-top">SIGNAL / STORY / CONTEXT</div><SignalOrb /><div className="orb-caption orb-caption-bottom"><span>✳</span> From raw footage to considered moments</div></div></section>
    <section id="intelligence" className="intelligence"><div className="section-head"><div><span className="section-kicker">THE DECISION SYSTEM</span><h2>Every break has a reason.</h2></div><p>One model-powered understanding layer. A strict safety layer. A reviewer in control.</p></div><div className="job-toolbar"><div><span className="live-dot" /><strong>LIVE ANALYSIS</strong><span>{loading ? "Loading jobs…" : jobs.length ? `${jobs.length} video${jobs.length === 1 ? "" : "s"} in library` : "No uploads yet"}</span></div><label htmlFor="job-select">VIDEO <select id="job-select" value={selectedId} onChange={(event) => setSelectedId(event.target.value)} disabled={!jobs.length}>{jobs.length ? jobs.map((item) => <option value={item.id} key={item.id}>{item.fileName}</option>) : <option value="">Upload in the studio to begin</option>}</select></label></div>{error && <p className="data-error" role="alert">{error} · The original studio is still available.</p>}{!job && !loading && !error && <div className="empty-state">No analysis yet. Upload a Bengali video in the <a href="/">studio</a>, then return here to see the story map.</div>}{job && <><Funnel transcript={transcript} review={job.review} /><div className="analysis-status"><span className="status-chip">{job.status.toUpperCase()}</span><span>{job.message || "Analysis is ready to explore."}</span><a href={studioLink}>Inspect or edit this analysis ↗</a></div></>}</section>
    <section id="story-map" className="story-section"><div className="section-head"><div><span className="section-kicker">A FILM, UNDERSTOOD</span><h2>The story has a rhythm.</h2></div><p>Explore the AI evidence behind each selected moment. These signals guide the recommendation; they never override safety.</p></div><div className="story-grid"><div className="story-main"><div className="story-card"><div className="card-title"><span>01 / EDITING RHYTHM</span><span>{transcript?.shot_boundaries?.length || 0} DETECTED CUTS</span></div>{duration > 0 ? <Timeline transcript={transcript} duration={duration} review={{ ...job?.review, jobId: job?.id }} /> : <div className="chart-empty">Choose an analysed video to reveal its shot and break timeline.</div>}</div><div className="story-card"><div className="card-title"><span>02 / EMOTIONAL LANDSCAPE</span><span>AI + AUDIO + EDITING</span></div><LineChart pacing={transcript?.pacing} /><div className="chart-axis"><span>OPENING</span><span>STORY INTENSITY →</span><span>ENDING</span></div></div></div><aside className="scene-panel"><div className="card-title"><span>03 / EXPLAINABLE CHOICES</span><span>{safeCandidates.length} POTENTIAL</span></div>{safeCandidates.length ? safeCandidates.slice(0, 4).map((item, index) => <div className="decision-card" key={item.candidate_id || index}><div><span className="decision-time">{time(item.time)}</span><span className={`decision-tier tier-${(item.tier || "medium").toLowerCase()}`}>{item.tier || "Potential"}</span></div><strong>{item.scene_context || "A natural scene change"}</strong><p>{item.ai_reason || item.selection_note || "AI evidence supports a low-disruption moment."}</p></div>) : <div className="chart-empty">No safe moments are available yet. Scene analysis may still be running—or the story is better left uninterrupted.</div>}<a className="panel-link" href={studioLink}>Review all cuts and ad options ↗</a></aside></div><div className="scene-strip">{scenes.slice(0, 4).map((scene, index) => <div key={scene.scene_id || index}><span>SCENE {String(index + 1).padStart(2, "0")} · {time(scene.start)}</span><strong>{scene.summary}</strong><small>{(scene.tone || []).slice(0, 3).join(" · ") || "Context understood"}</small></div>)}</div></section>
    <section className="closing"><span className="section-kicker">STORY FIRST. ADS SECOND.</span><h2>Make every break<br /><em>earn its place.</em></h2><p>AI finds the possibilities. Policy protects the viewer. You make the final call.</p><a className="button-primary" href={studioLink}>Open the full studio <span>↗</span></a></section><footer><span>✳ SCENESENSE / HOICHOI HACKATHON 2026</span><span>BUILT FOR THE MOMENT</span></footer>
  </main>;
}
