(function () {
  "use strict";

  htmx.config.timeout = 10000;
  htmx.config.allowEval = false;

  var pendingRequests = new Map();
  var abortedXhrs = new WeakSet();
  var consecutiveErrors = 0;
  // First retry is fast: 503 on /api/stream is almost always the daemon
  // releasing its single ffmpeg slot from the previous request. Capping
  // the backoff low keeps the preview from staying black for half a
  // minute if the doubler escalates: the daemon now write-timeouts
  // stuck slots within 5 s, so longer waits cannot help.
  var streamRetryDelay = 500;
  var maxStreamRetryDelay = 5000;
  var sliderInteracting = false;
  var sliderInteractEndTimer = null;

  function markSliderInteracting() {
    sliderInteracting = true;
    if (sliderInteractEndTimer) {
      clearTimeout(sliderInteractEndTimer);
      sliderInteractEndTimer = null;
    }
  }

  function endSliderInteractingSoon() {
    if (sliderInteractEndTimer) clearTimeout(sliderInteractEndTimer);
    sliderInteractEndTimer = setTimeout(function () {
      sliderInteracting = false;
      sliderInteractEndTimer = null;
    }, 800);
  }

  function isPtzSliderTarget(t) {
    return t && t.classList && t.classList.contains("ptz-slider");
  }

  document.addEventListener("pointerdown", function (e) {
    if (isPtzSliderTarget(e.target)) markSliderInteracting();
  }, true);
  document.addEventListener("pointerup", function (e) {
    if (isPtzSliderTarget(e.target)) endSliderInteractingSoon();
  }, true);
  document.addEventListener("pointercancel", function (e) {
    if (isPtzSliderTarget(e.target)) endSliderInteractingSoon();
  }, true);
  document.addEventListener("focusin", function (e) {
    if (isPtzSliderTarget(e.target)) markSliderInteracting();
  });
  document.addEventListener("focusout", function (e) {
    if (isPtzSliderTarget(e.target)) endSliderInteractingSoon();
  });

  document.body.addEventListener("doAction", function (e) {
    htmx.ajax("POST", e.detail.url, {
      target: "#status-panel",
      swap: "outerHTML",
    });
  });

  function clamp(v, lo, hi) {
    return Math.max(lo, Math.min(hi, v));
  }

  document.addEventListener("click", function (e) {
    var t = e.target.closest("[data-axis], [data-action]");
    if (!t || !t.classList.contains("ptz-pad-wedge") && !t.classList.contains("ptz-pad-home")) return;
    var action = t.getAttribute("data-action");
    if (action === "center") {
      htmx.ajax("POST", "/api/center", {
        target: "#status-panel",
        swap: "outerHTML",
      });
      return;
    }
    var axis = t.getAttribute("data-axis");
    var delta = parseInt(t.getAttribute("data-delta"), 10);
    if (!axis || isNaN(delta)) return;
    var slider = document.getElementById("slider-" + axis);
    if (!slider) return;
    var current = parseInt(slider.value, 10) || 0;
    var lo = parseInt(slider.min, 10);
    var hi = parseInt(slider.max, 10);
    var next = clamp(current + delta, lo, hi);
    if (next === current) return;
    slider.value = next;
    var valEl = document.getElementById("val-" + axis);
    if (valEl) {
      var suffix = axis === "zoom" ? "x" : "°";
      valEl.textContent = next + suffix;
    }
    htmx.ajax("POST", "/api/ptz/" + axis, {
      target: "#ptz-" + axis,
      swap: "outerHTML",
      values: { value: String(next) },
    });
  });

  document.addEventListener("htmx:configRequest", function (e) {
    var pathInfo = e.detail.pathInfo;
    if (!pathInfo) return;
    var path = pathInfo.requestPath;
    if (path.indexOf("/api/ptz/") === 0) {
      var elt = e.detail.elt;
      if (elt && elt.classList.contains("ptz-slider")) {
        e.detail.parameters.value = elt.value;
      }
    }
  });

  document.addEventListener("input", function (e) {
    if (!e.target.classList.contains("ptz-slider")) return;
    var axis = e.target.id.replace("slider-", "");
    var valEl = document.getElementById("val-" + axis);
    if (!valEl) return;
    var suffix = axis === "zoom" ? "x" : "\u00b0";
    valEl.textContent = e.target.value + suffix;
  });

  function showToast(msg, type) {
    type = type || "success";
    var container = document.getElementById("toast-container");
    container.innerHTML = '<div class="toast toast-' + type + ' show">' + msg + "</div>";
    setTimeout(function () {
      var el = container.querySelector(".toast");
      if (el) {
        el.classList.remove("show");
        setTimeout(function () {
          el.remove();
        }, 300);
      }
    }, 2500);
  }

  function showStickyToast(id, msg, type) {
    type = type || "error";
    var container = document.getElementById("toast-container");
    if (!container) return;
    var existing = container.querySelector('[data-sticky="' + id + '"]');
    if (existing) {
      existing.textContent = msg;
      return;
    }
    var el = document.createElement("div");
    el.className = "toast toast-" + type + " show";
    el.setAttribute("data-sticky", id);
    el.textContent = msg;
    container.appendChild(el);
  }

  function hideStickyToast(id) {
    var container = document.getElementById("toast-container");
    if (!container) return;
    var el = container.querySelector('[data-sticky="' + id + '"]');
    if (!el) return;
    el.classList.remove("show");
    setTimeout(function () {
      if (el.parentNode) el.parentNode.removeChild(el);
    }, 300);
  }

  document.addEventListener("htmx:beforeRequest", function (e) {
    var path = e.detail.pathInfo && e.detail.pathInfo.requestPath;
    if (path === "/panel" && document.visibilityState !== "visible") {
      abortedXhrs.add(e.detail.xhr);
      e.detail.xhr.abort();
      return;
    }
    if (path === "/panel" && sliderInteracting) {
      abortedXhrs.add(e.detail.xhr);
      e.detail.xhr.abort();
      return;
    }
    if (path && pendingRequests.has(path)) {
      var oldXhr = pendingRequests.get(path);
      if (oldXhr) {
        abortedXhrs.add(oldXhr);
        try { oldXhr.abort(); } catch (_) {}
      }
    }
    if (path) pendingRequests.set(path, e.detail.xhr);
    if (path && path.indexOf("/api/ptz/") === 0) {
      var axis = path.split("/").pop();
      var slider = document.getElementById("slider-" + axis);
      if (slider) slider.classList.add("sending");
    }
  });

  document.addEventListener("htmx:afterRequest", function (e) {
    var path = e.detail.pathInfo && e.detail.pathInfo.requestPath;

    if (path) {
      if (pendingRequests.get(path) === e.detail.xhr) {
        pendingRequests.delete(path);
      }
      if (path.indexOf("/api/ptz/") === 0) {
        var axis = path.split("/").pop();
        var slider = document.getElementById("slider-" + axis);
        if (slider) slider.classList.remove("sending");
      }
    }

    if (e.detail.failed) {
      if (e.detail.xhr && abortedXhrs.has(e.detail.xhr)) {
        return;
      }
      if (e.detail.xhr && e.detail.xhr.status === 0) {
        return;
      }
      consecutiveErrors++;
      if (path && path.indexOf("/api/ptz/") === 0) {
        var errAxis = path.split("/").pop();
        var errSlider = document.getElementById("slider-" + errAxis);
        if (errSlider && errSlider.dataset.lastGood !== undefined) {
          errSlider.value = errSlider.dataset.lastGood;
          var valEl = document.getElementById("val-" + errAxis);
          if (valEl) {
            var suffix = errAxis === "zoom" ? "x" : "\u00b0";
            valEl.textContent = errSlider.dataset.lastGood + suffix;
          }
        }
      }
      if (consecutiveErrors >= 3) {
        showStickyToast("offline", "Daemon unreachable \u2014 reconnecting\u2026", "error");
      } else {
        showToast("Request failed", "error");
      }
      return;
    }

    consecutiveErrors = 0;
    hideStickyToast("offline");
    hideStickyToast("connection-error");

    if (path && path.indexOf("/api/ptz/") === 0) {
      var okAxis = path.split("/").pop();
      var okSlider = document.getElementById("slider-" + okAxis);
      if (okSlider) okSlider.dataset.lastGood = okSlider.value;
    }

    // Note: no preview reload triggered from here. The daemon publishes
    // a PTZ SSE event after handlePTZ commits, and the inner IIFE's
    // scheduleRefresh swaps preview-section in response. Triggering
    // another reload from this handler stacks 2-3 reconnects on the
    // daemon's size-1 stream semaphore and lands one of them on 503,
    // which then drops into the slow exponential backoff (black screen).
  });

  document.addEventListener("htmx:responseError", function (e) {
    if (e.detail && e.detail.xhr && abortedXhrs.has(e.detail.xhr)) return;
    showStickyToast("connection-error", "Connection error \u2014 will retry automatically", "error");
  });

  document.addEventListener("htmx:timeout", function () {
    showToast("Request timed out", "error");
  });

  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible") {
      streamRetryDelay = 3000;
    }
  });

  document.addEventListener("keydown", function (e) {
    if (
      e.target.tagName === "INPUT" ||
      e.target.tagName === "TEXTAREA" ||
      e.target.tagName === "SELECT"
    )
      return;
    var map = {
      t: "/api/track",
      i: "/api/idle",
      p: "/api/privacy",
      c: "/api/center",
    };
    var url = map[e.key.toLowerCase()];
    if (!url) return;
    e.preventDefault();
    var badge = document.querySelector(".header-badge");
    if (badge && badge.textContent === "Offline") {
      showToast("Camera offline", "error");
      return;
    }
    htmx.trigger(document.body, "doAction", { url: url });
  });

  (function () {
    if (!window.EventSource) return;
    var es = null;
    var retryDelay = 1000;
    var maxRetryDelay = 30000;
    var refreshDebounceTimer = null;

    function scheduleRefresh() {
      if (refreshDebounceTimer) return;
      refreshDebounceTimer = setTimeout(function () {
        refreshDebounceTimer = null;
        if (document.visibilityState !== "visible") return;
        htmx.trigger(document.body, "refresh");
      }, 50);
    }

    // refreshPreviewSection swaps just the preview-section card. Reserve
    // for transitions that actually change what is rendered in that card
    // (privacy on/off, online flag, paused toggle); any other reason
    // tears down the <img> and races the daemon's stream semaphore.
    function refreshPreviewSection() {
      var preview = document.getElementById("preview-section");
      if (!preview) return;
      // Fire the dedicated pixy:previewRefresh event rather than calling
      // htmx.ajax directly: preview-section subscribes to it via the
      // template's hx-trigger so the swap goes through htmx's
      // request-de-duplication.
      htmx.trigger(document.body, "pixy:previewRefresh");
    }

    var lastCamera = null;
    var lastOnline = null;

    function connect() {
      try {
        es = new EventSource("/api/events");
      } catch (err) {
        return;
      }
      es.addEventListener("state", function (e) {
        // Always nudge non-preview hx-trigger="refresh" listeners so the
        // status panel reflects audio mode / gesture / auto / inCall.
        scheduleRefresh();

        var data;
        try {
          data = JSON.parse(e.data);
        } catch (err) {
          // Couldn't decode; fall back to the conservative full refresh.
          refreshPreviewSection();
          return;
        }
        if (!data) return;

        // Only swap preview-section when the card's rendered content can
        // actually change. The card depends on whether the preview is
        // displayable: online && camera !== "privacy". Track <-> idle <->
        // sleep <-> scanning all render the same live <img>, so a swap on
        // those transitions only tears the <img> down and races the
        // daemon's stream semaphore. PreviewPaused is server-driven
        // through /api/preview/toggle which already swaps preview-section
        // via hx-target, so it does not need to fire from here.
        var firstEvent = lastCamera === null && lastOnline === null;
        var wasDisplayable = !firstEvent && lastCamera !== "privacy" && lastOnline;
        var nowDisplayable = data.camera !== "privacy" && data.online;
        if (!firstEvent && wasDisplayable !== nowDisplayable) {
          refreshPreviewSection();
        }

        var leftPrivacy = lastCamera === "privacy" && data.camera && data.camera !== "privacy";
        lastCamera = data.camera;
        lastOnline = data.online;

        // When the camera leaves privacy, nudge the preview reload path so
        // the new stream is fetched even if the swap above is already in
        // flight or skipped (e.g. previewPaused gating).
        if (leftPrivacy) {
          htmx.trigger(document.body, "pixy:previewReset");
        }
      });
      // PTZ events do NOT need a preview-section swap: the camera has
      // already moved and the in-flight MJPEG stream will reflect the new
      // framing on its next frame. Re-swapping preview-section tears down
      // the <img>, races the daemon's stream semaphore release, and one
      // of every few attempts lands on a 503 ("stream already in use")
      // which then drops the user into a 3 s exponential-backoff black
      // screen. Just nudge other status-panel listeners so any slider
      // value catches up if the change came from outside (tray / CLI).
      es.addEventListener("ptz", function () {
        htmx.trigger(document.body, "refresh");
      });
      es.addEventListener("online", scheduleRefresh);
      es.onopen = function () {
        retryDelay = 1000;
      };
      es.onerror = function () {
        if (es) {
          es.close();
          es = null;
        }
        setTimeout(connect, retryDelay);
        retryDelay = Math.min(retryDelay * 2, maxRetryDelay);
      };
    }
    connect();
  })();

  (function () {
    var img = document.getElementById("preview-img");
    if (!img) return;
    var retryTimer = null;

    function streamShouldBeBlocked() {
      // Camera state and pause state both render into the status panel; reading
      // the DOM avoids extra requests. If the state indicator says "privacy" or
      // the preview section has the paused placeholder, the server will reject
      // /api/stream with 503, so don't bother retrying \u2014 the SSE state event
      // will fire pixy:previewReset / refresh once the gate lifts.
      var stateEl = document.querySelector(".state-indicator");
      if (stateEl && stateEl.classList.contains("state-privacy")) return true;
      var fallback = document.getElementById("preview-fallback");
      if (fallback && fallback.dataset.reason === "paused") return true;
      return false;
    }

    function reloadPreview(delay) {
      if (retryTimer) {
        clearTimeout(retryTimer);
        retryTimer = null;
      }
      if (streamShouldBeBlocked()) return;
      var fallback = document.getElementById("preview-fallback");
      retryTimer = setTimeout(function () {
        retryTimer = null;
        if (streamShouldBeBlocked()) return;
        img.src = "/api/stream?" + Date.now();
        img.style.display = "";
        if (fallback) fallback.style.display = "none";
      }, delay);
    }

    img.addEventListener("load", function () {
      streamRetryDelay = 500;
    });

    img.addEventListener("error", function () {
      if (retryTimer) return;
      this.style.display = "none";
      var fallback = document.getElementById("preview-fallback");
      if (fallback) {
        fallback.style.display = "flex";
        var label = fallback.querySelector("div:last-child");
        if (label) {
          if (streamShouldBeBlocked()) {
            label.textContent = "Preview blocked (privacy or paused)";
          } else {
            label.textContent = "Reconnecting\u2026";
          }
        }
      }
      if (streamShouldBeBlocked()) return;
      reloadPreview(streamRetryDelay);
      streamRetryDelay = Math.min(streamRetryDelay * 2, maxStreamRetryDelay);
    });

    document.body.addEventListener("pixy:previewReset", function () {
      streamRetryDelay = 500;
      reloadPreview(500);
    });
  })();
})();
