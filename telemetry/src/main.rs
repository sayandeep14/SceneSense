use std::collections::{BTreeMap, HashMap, VecDeque};
use std::env;
use std::fmt::Write as _;
use std::fs;
use std::net::UdpSocket;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const MAX_PACKET: usize = 512;
const MAX_RECENT: usize = 32;
const MAX_DURATION_MS: u64 = 86_400_000;

#[derive(Default)]
struct Metric {
    count: u64,
    errors: u64,
    total_ms: u64,
    max_ms: u64,
}

struct Recent {
    job_id: String,
    name: String,
    status: String,
    duration_ms: u64,
    at_ms: u64,
}

#[derive(Default)]
struct Observer {
    received: u64,
    rejected: u64,
    metrics: BTreeMap<String, Metric>,
    event_counts: BTreeMap<String, u64>,
    stages: HashMap<String, (String, Instant)>,
    recent: VecDeque<Recent>,
}

fn label(value: &str, max: usize) -> bool {
    !value.is_empty()
        && value.len() <= max
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_' || byte == b'-')
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

impl Observer {
    fn metric(&mut self, job: &str, name: &str, status: &str, duration_ms: u64) {
        let entry = self.metrics.entry(name.to_string()).or_default();
        entry.count = entry.count.saturating_add(1);
        entry.errors = entry.errors.saturating_add(u64::from(status == "error"));
        entry.total_ms = entry.total_ms.saturating_add(duration_ms);
        entry.max_ms = entry.max_ms.max(duration_ms);
        self.push_recent(job, name, status, duration_ms);
    }

    fn push_recent(&mut self, job: &str, name: &str, status: &str, duration_ms: u64) {
        if self.recent.len() == MAX_RECENT {
            self.recent.pop_front();
        }
        self.recent.push_back(Recent {
            job_id: job.to_string(),
            name: name.to_string(),
            status: status.to_string(),
            duration_ms,
            at_ms: now_ms(),
        });
    }

    fn process(&mut self, packet: &[u8], instant: Instant) -> bool {
        let Ok(text) = std::str::from_utf8(packet) else {
            self.rejected += 1;
            return false;
        };
        let fields: Vec<&str> = text.split('|').collect();
        let valid = match fields.as_slice() {
            ["S", job, stage] if label(job, 40) && label(stage, 48) => {
                if let Some((previous, started)) = self
                    .stages
                    .insert((*job).to_string(), ((*stage).to_string(), instant))
                {
                    self.metric(
                        job,
                        &previous,
                        "ok",
                        instant.duration_since(started).as_millis() as u64,
                    );
                }
                true
            }
            ["F", job, status] if label(job, 40) && (*status == "ok" || *status == "error") => {
                if let Some((previous, started)) = self.stages.remove(*job) {
                    self.metric(
                        job,
                        &previous,
                        status,
                        instant.duration_since(started).as_millis() as u64,
                    );
                }
                *self
                    .event_counts
                    .entry(format!("analysis_{status}"))
                    .or_default() += 1;
                self.push_recent(job, "analysis", status, 0);
                true
            }
            ["M", job, name, status, value]
                if label(job, 40) && label(name, 48) && (*status == "ok" || *status == "error") =>
            {
                match value.parse::<u64>() {
                    Ok(duration_ms) if duration_ms <= MAX_DURATION_MS => {
                        self.metric(job, name, status, duration_ms);
                        true
                    }
                    _ => false,
                }
            }
            ["E", job, name] if label(job, 40) && label(name, 48) => {
                *self.event_counts.entry((*name).to_string()).or_default() += 1;
                self.push_recent(job, name, "event", 0);
                true
            }
            _ => false,
        };
        if valid {
            self.received += 1;
        } else {
            self.rejected += 1;
        }
        valid
    }

    fn snapshot(&self) -> String {
        let mut out = format!(
            "{{\"generated_at_ms\":{},\"events_received\":{},\"packets_rejected\":{},\"metrics\":{{",
            now_ms(),
            self.received,
            self.rejected
        );
        for (index, (name, metric)) in self.metrics.iter().enumerate() {
            if index > 0 {
                out.push(',');
            }
            let _ = write!(
                out,
                "\"{name}\":{{\"count\":{},\"errors\":{},\"total_ms\":{},\"max_ms\":{}}}",
                metric.count, metric.errors, metric.total_ms, metric.max_ms
            );
        }
        out.push_str("},\"event_counts\":{");
        for (index, (name, count)) in self.event_counts.iter().enumerate() {
            if index > 0 {
                out.push(',');
            }
            let _ = write!(out, "\"{name}\":{count}");
        }
        out.push_str("},\"recent\":[");
        for (index, event) in self.recent.iter().enumerate() {
            if index > 0 {
                out.push(',');
            }
            let _ = write!(
                out,
                "{{\"job_id\":\"{}\",\"name\":\"{}\",\"status\":\"{}\",\"duration_ms\":{},\"at_ms\":{}}}",
                event.job_id, event.name, event.status, event.duration_ms, event.at_ms
            );
        }
        out.push_str("]}");
        out
    }
}

fn write_snapshot(path: &Path, content: &str) -> std::io::Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let temporary = path.with_extension("json.tmp");
    fs::write(&temporary, content)?;
    fs::rename(temporary, path)
}

fn main() -> std::io::Result<()> {
    let path = PathBuf::from(
        env::args()
            .nth(1)
            .unwrap_or_else(|| "data/uploads/observability.json".to_string()),
    );
    let socket = UdpSocket::bind("127.0.0.1:9917")?;
    socket.set_read_timeout(Some(Duration::from_secs(30)))?;
    let mut observer = Observer::default();
    if let Err(error) = write_snapshot(&path, &observer.snapshot()) {
        eprintln!("observer snapshot unavailable: {error}");
    }
    let mut packet = [0u8; MAX_PACKET];
    loop {
        match socket.recv_from(&mut packet) {
            Ok((size, _)) => {
                if size == MAX_PACKET {
                    observer.rejected = observer.rejected.saturating_add(1);
                    continue;
                }
                if observer.process(&packet[..size], Instant::now()) {
                    println!(
                        "{{\"component\":\"rust_observer\",\"event\":\"telemetry_accepted\",\"received\":{}}}",
                        observer.received
                    );
                    if let Err(error) = write_snapshot(&path, &observer.snapshot()) {
                        eprintln!("observer snapshot unavailable: {error}");
                    }
                }
            }
            Err(error)
                if error.kind() == std::io::ErrorKind::WouldBlock
                    || error.kind() == std::io::ErrorKind::TimedOut =>
            {
                if let Err(error) = write_snapshot(&path, &observer.snapshot()) {
                    eprintln!("observer snapshot unavailable: {error}");
                }
            }
            Err(error) => eprintln!("observer socket error: {error}"),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stages_and_metrics_are_aggregated_without_accepting_untrusted_fields() {
        let mut observer = Observer::default();
        let start = Instant::now();
        assert!(observer.process(b"S|job-1|transcribing", start));
        assert!(observer.process(
            b"S|job-1|detecting_shots",
            start + Duration::from_millis(125)
        ));
        assert!(observer.process(b"F|job-1|ok", start + Duration::from_millis(325)));
        assert!(observer.process(b"M|job-1|upload|ok|70", start));
        assert!(observer.process(b"E|job-1|ad_start", start));
        assert!(!observer.process(b"M|../../path|upload|ok|20", start));
        assert!(!observer.process(b"E|job-1|ad_start\nsecret", start));
        assert_eq!(observer.metrics["transcribing"].total_ms, 125);
        assert_eq!(observer.metrics["detecting_shots"].total_ms, 200);
        assert_eq!(observer.metrics["upload"].count, 1);
        assert_eq!(observer.event_counts["ad_start"], 1);
        assert_eq!(observer.rejected, 2);
        assert!(observer.snapshot().contains("\"analysis_ok\":1"));
    }
}
