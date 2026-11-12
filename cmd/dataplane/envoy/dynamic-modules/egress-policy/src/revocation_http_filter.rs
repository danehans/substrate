// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//! Guards the outer CONNECT while internal listener inspection is pending.
use envoy_proxy_dynamic_modules_rust_sdk::{abi, *};
use std::collections::HashMap;
use std::sync::{
  Arc, Mutex,
  atomic::{AtomicBool, AtomicU64, Ordering},
};
use std::time::{Duration, Instant};

const TOKEN: &[u8] = b"dev.ate.revocation.token";
const TICK: Duration = Duration::from_millis(100);
const LOCAL_POLL: Duration = Duration::from_millis(500);
const FRESHNESS: Duration = Duration::from_secs(7);

struct ScheduledHandle {
  scheduler: Box<dyn EnvoyHttpFilterScheduler>,
  pending: Arc<AtomicBool>,
}
impl ScheduledHandle {
  fn schedule(&self) {
    if !self.pending.swap(true, Ordering::AcqRel) {
      self.scheduler.commit(0);
    }
  }
}
type Schedulers = Arc<Mutex<HashMap<u64, ScheduledHandle>>>;
struct Config {
  schedulers: Schedulers,
  stop: Arc<AtomicBool>,
  thread: Option<std::thread::JoinHandle<()>>,
  next: AtomicU64,
  closed: EnvoyCounterId,
}
impl Drop for Config {
  fn drop(&mut self) {
    self.stop.store(true, Ordering::Release);
    if let Some(thread) = self.thread.take() {
      let _ = thread.join();
    }
  }
}
impl<ENF: EnvoyHttpFilter> HttpFilterConfig<ENF> for Config {
  fn new_http_filter(&self, _: &mut ENF) -> Box<dyn HttpFilter<ENF>> {
    Box::new(Filter {
      id: self.next.fetch_add(1, Ordering::Relaxed),
      schedulers: self.schedulers.clone(),
      token: None,
      deadline: Instant::now() + FRESHNESS,
      next: Instant::now(),
      sent: None,
      started: false,
      pending: Arc::new(AtomicBool::new(false)),
      closed: false,
      close_counter: self.closed,
    })
  }
}
struct Filter {
  id: u64,
  schedulers: Schedulers,
  token: Option<String>,
  deadline: Instant,
  next: Instant,
  sent: Option<Instant>,
  started: bool,
  pending: Arc<AtomicBool>,
  closed: bool,
  close_counter: EnvoyCounterId,
}
impl Drop for Filter {
  fn drop(&mut self) {
    self.schedulers.lock().unwrap().remove(&self.id);
  }
}
impl Filter {
  fn close<ENF: EnvoyHttpFilter>(&mut self, envoy: &mut ENF, reason: &str) {
    if self.closed {
      return;
    }
    self.closed = true;
    let _ = envoy.increment_counter(self.close_counter, 1);
    self.schedulers.lock().unwrap().remove(&self.id);
    let identity = envoy
      .get_filter_state_bytes(b"dev.ate.revocation.identity")
      .map(|v| String::from_utf8_lossy(v.as_slice()).into_owned())
      .unwrap_or_default();
    envoy_log_warn!(
      "connect_revocation reason={} identity={} connection_id={}",
      reason,
      identity,
      envoy.get_upstream_connection_id()
    );
    envoy.reset_stream(
      abi::envoy_dynamic_module_type_http_filter_stream_reset_reason::LocalReset,
      reason,
    );
  }
  fn check<ENF: EnvoyHttpFilter>(&mut self, envoy: &mut ENF) {
    if self.closed {
      return;
    }
    let now = Instant::now();
    if now >= self.deadline {
      self.close(envoy, "actor-check-overdue");
      return;
    }
    if self.sent.is_some() || now < self.next {
      return;
    }
    let Some(token) = &self.token else { return };
    let authorization = format!("Bearer {}", token);
    self.sent = Some(now);
    let timeout = self
      .deadline
      .saturating_duration_since(now)
      .min(Duration::from_secs(2))
      .as_millis()
      .max(1) as u64;
    let (result, _) = envoy.send_http_callout(
      "revocation_control",
      &[
        (":method", b"POST"),
        (":path", b"/permit"),
        ("host", b"revocation-control"),
        ("authorization", authorization.as_bytes()),
        ("x-permit-phase", b"connect"),
      ],
      None,
      timeout,
    );
    if result != abi::envoy_dynamic_module_type_http_callout_init_result::Success {
      self.close(envoy, "actor-check-failed");
    }
  }
}
impl<ENF: EnvoyHttpFilter> HttpFilter<ENF> for Filter {
  fn on_request_headers(
    &mut self,
    envoy: &mut ENF,
    _end_of_stream: bool,
  ) -> abi::envoy_dynamic_module_type_on_http_filter_request_headers_status {
    let mode = envoy
      .get_filter_state_bytes(b"dev.ate.revocation.mode")
      .map(|v| v.as_slice().to_vec());
    if mode.as_deref() != Some(b"required") {
      self.close(envoy, "actor-check-failed");
      return abi::envoy_dynamic_module_type_on_http_filter_request_headers_status::StopIteration;
    }
    let token = envoy
      .get_filter_state_bytes(TOKEN)
      .map(|v| v.as_slice().to_vec());
    let Some(token) = token else {
      self.close(envoy, "actor-check-failed");
      return abi::envoy_dynamic_module_type_on_http_filter_request_headers_status::StopIteration;
    };
    let Ok(token) = String::from_utf8(token) else {
      self.close(envoy, "actor-check-failed");
      return abi::envoy_dynamic_module_type_on_http_filter_request_headers_status::StopIteration;
    };
    if token.len() != 64 || !token.bytes().all(|b| b.is_ascii_hexdigit()) {
      self.close(envoy, "actor-check-failed");
      return abi::envoy_dynamic_module_type_on_http_filter_request_headers_status::StopIteration;
    }
    self.token = Some(token);
    self.schedulers.lock().unwrap().insert(
      self.id,
      ScheduledHandle {
        scheduler: Box::new(envoy.new_scheduler()),
        pending: self.pending.clone(),
      },
    );
    self.check(envoy);
    abi::envoy_dynamic_module_type_on_http_filter_request_headers_status::StopIteration
  }
  fn on_scheduled(&mut self, envoy: &mut ENF, _: u64) {
    self.pending.store(false, Ordering::Release);
    self.check(envoy);
  }
  fn on_http_callout_done(
    &mut self,
    envoy: &mut ENF,
    _: u64,
    result: abi::envoy_dynamic_module_type_http_callout_result,
    headers: Option<&[(EnvoyBuffer, EnvoyBuffer)]>,
    body: Option<&[EnvoyBuffer]>,
  ) {
    if self.closed {
      return;
    }
    let Some(sent) = self.sent.take() else {
      self.close(envoy, "actor-check-failed");
      return;
    };
    if Instant::now() >= self.deadline {
      self.close(envoy, "actor-check-overdue");
      return;
    }
    let headers = headers.unwrap_or_default();
    let body = body.unwrap_or_default();
    let status = headers
      .iter()
      .find(|(k, _)| k.as_slice() == b":status")
      .map(|(_, v)| v.as_slice());
    let bytes: Vec<u8> = body.iter().flat_map(|v| v.as_slice()).copied().collect();
    if result != abi::envoy_dynamic_module_type_http_callout_result::Success
      || status != Some(b"200")
    {
      let reason = match std::str::from_utf8(&bytes).unwrap_or("").trim() {
        "actor-uid-mismatch" => "actor-uid-mismatch",
        "activation-mismatch" => "activation-mismatch",
        "actor-quarantined" => "actor-quarantined",
        "actor-check-overdue" => "actor-check-overdue",
        _ => "actor-check-failed",
      };
      self.close(envoy, reason);
      return;
    }
    let remaining = std::str::from_utf8(&bytes)
      .ok()
      .and_then(|s| s.parse::<u64>().ok())
      .filter(|v| *v > 0 && *v <= 7000);
    let Some(remaining) = remaining else {
      self.close(envoy, "actor-check-failed");
      return;
    };
    self.deadline = sent + Duration::from_millis(remaining);
    if Instant::now() >= self.deadline {
      self.close(envoy, "actor-check-overdue");
      return;
    }
    self.next = Instant::now() + LOCAL_POLL;
    if !self.started {
      self.started = true;
      envoy.continue_decoding();
    }
  }
  fn on_stream_complete(&mut self, envoy: &mut ENF) {
    self.closed = true;
    self.sent = None;
    self.schedulers.lock().unwrap().remove(&self.id);
    if let Some(token) = self.token.take() {
      let authorization = format!("Bearer {}", token);
      let _ = envoy.send_http_callout(
        "revocation_control",
        &[
          (":method", b"DELETE"),
          (":path", b"/permit"),
          ("host", b"revocation-control"),
          ("authorization", authorization.as_bytes()),
          ("x-permit-phase", b"connect"),
        ],
        None,
        1000,
      );
    }
  }
}

pub fn new_http_filter_config_fn<EC: EnvoyHttpFilterConfig, ENF: EnvoyHttpFilter>(
  config: &mut EC,
  _: &str,
  _: &[u8],
) -> Option<Box<dyn HttpFilterConfig<ENF>>> {
  let closed = config
    .define_counter("ate_egress.revocation.stream_closed")
    .ok()?;
  let schedulers: Schedulers = Arc::new(Mutex::new(HashMap::new()));
  let stop = Arc::new(AtomicBool::new(false));
  let registry = schedulers.clone();
  let stopped = stop.clone();
  let thread = std::thread::spawn(move || {
    while !stopped.load(Ordering::Acquire) {
      std::thread::sleep(TICK);
      if stopped.load(Ordering::Acquire) {
        break;
      }
      for scheduler in registry.lock().unwrap().values() {
        scheduler.schedule();
      }
    }
  });
  Some(Box::new(Config {
    schedulers,
    stop,
    thread: Some(thread),
    next: AtomicU64::new(1),
    closed,
  }))
}

#[cfg(test)]
mod tests {
  use super::*;
  fn filter() -> Filter {
    Filter {
      id: 1,
      schedulers: Arc::new(Mutex::new(HashMap::new())),
      token: None,
      deadline: Instant::now() + FRESHNESS,
      next: Instant::now(),
      sent: None,
      started: false,
      pending: Arc::new(AtomicBool::new(false)),
      closed: false,
      close_counter: EnvoyCounterId(0),
    }
  }
  #[test]
  fn missing_required_state_fails_closed() {
    for mode in [None, Some(b"required".as_slice()), Some(b"legacy".as_slice())] {
      let mut envoy = MockEnvoyHttpFilter::new();
      envoy.expect_get_filter_state_bytes().returning(move |key| {
        if key == b"dev.ate.revocation.mode" {
          mode.map(EnvoyBuffer::new)
        } else {
          None
        }
      });
      envoy
        .expect_increment_counter()
        .times(1)
        .returning(|_, _| Ok(()));
      envoy.expect_get_upstream_connection_id().return_const(1u64);
      envoy
        .expect_reset_stream()
        .withf(|kind, reason| {
          *kind == abi::envoy_dynamic_module_type_http_filter_stream_reset_reason::LocalReset
            && reason == "actor-check-failed"
        })
        .times(1)
        .return_const(());
      let mut filter = filter();
      assert_eq!(
        filter.on_request_headers(&mut envoy, false),
        abi::envoy_dynamic_module_type_on_http_filter_request_headers_status::StopIteration
      );
      // A scheduled or late response callback after reset cannot restart checks.
      filter.on_scheduled(&mut envoy, 0);
      filter.on_http_callout_done(
        &mut envoy,
        1,
        abi::envoy_dynamic_module_type_http_callout_result::Success,
        None,
        None,
      );
    }
  }

  #[test]
  fn completion_fences_late_callbacks() {
    let mut envoy = MockEnvoyHttpFilter::new();
    envoy
      .expect_send_http_callout()
      .times(1)
      .returning(|_, headers, _, _| {
        assert!(
          headers
            .iter()
            .any(|(key, value)| *key == ":method" && *value == b"DELETE")
        );
        (
          abi::envoy_dynamic_module_type_http_callout_init_result::Success,
          1,
        )
      });
    let mut filter = filter();
    filter.token = Some("a".repeat(64));
    filter.sent = Some(Instant::now());
    filter.on_stream_complete(&mut envoy);
    filter.on_scheduled(&mut envoy, 0);
    let headers = [(EnvoyBuffer::new(b":status"), EnvoyBuffer::new(b"200"))];
    let body = [EnvoyBuffer::new(b"5000")];
    filter.on_http_callout_done(
      &mut envoy,
      1,
      abi::envoy_dynamic_module_type_http_callout_result::Success,
      Some(&headers),
      Some(&body),
    );
    assert!(filter.closed);
    assert!(filter.token.is_none());
  }
}
