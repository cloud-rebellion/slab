/* atomicsite circuit-board hero background. Vanilla JS, no deps, ~3.5KB.
   Triggered when a block has data-bg="circuit". The block renderer also
   emits a <canvas data-circuit-canvas></canvas> inside the section; this
   script finds it on DOMContentLoaded, draws an organic PCB-style circuit
   pattern in the site's primary color, and animates data signals along
   traces. Pauses when off-screen via IntersectionObserver. Honours
   prefers-reduced-motion: reduce by drawing one static frame.
   Adapted from brightinteraction.com/src/scripts/hero-circuit.ts (MIT). */
(function () {
  function rgbFromCss(varName, fallback) {
    var raw = getComputedStyle(document.documentElement).getPropertyValue(varName).trim() || fallback;
    if (raw[0] === '#') {
      var h = raw.slice(1);
      if (h.length === 3) h = h[0]+h[0]+h[1]+h[1]+h[2]+h[2];
      var i = parseInt(h, 16);
      return { r: (i >> 16) & 255, g: (i >> 8) & 255, b: i & 255 };
    }
    var m = raw.match(/(\d+)[, ]+(\d+)[, ]+(\d+)/);
    if (m) return { r: +m[1], g: +m[2], b: +m[3] };
    return { r: 14, g: 116, b: 144 }; // teal fallback
  }
  function init(canvas) {
    var ctx = canvas.getContext('2d');
    if (!ctx) return;
    var color = rgbFromCss('--color-primary', '#0E7490');
    var width = 0, height = 0;
    var nodes = [], traces = [], signals = [];
    var isMobile = window.innerWidth < 768;
    var prefersReduced = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    var MAX = isMobile ? 5 : 15;

    function generate() {
      nodes = []; traces = [];
      var spacing = Math.max(80, Math.min(120, width / 14));
      var cols = Math.floor(width / spacing) + 2;
      var rows = Math.floor(height / spacing) + 2;
      var ox = (width - (cols - 1) * spacing) / 2;
      var oy = (height - (rows - 1) * spacing) / 2;
      var cx = width / 2, cy = height / 2;
      var centerR = Math.min(width, height) * 0.32;
      for (var r = 0; r < rows; r++) {
        for (var c = 0; c < cols; c++) {
          if (Math.random() < 0.6) continue;
          var nx = ox + c * spacing;
          var ny = oy + r * spacing;
          if (Math.sqrt((nx - cx) * (nx - cx) + (ny - cy) * (ny - cy)) < centerR && Math.random() < 0.85) continue;
          var jx = (Math.random() - 0.5) * spacing * 0.3;
          var jy = (Math.random() - 0.5) * spacing * 0.3;
          var rnd = Math.random();
          var type = rnd < 0.1 ? 'chip' : rnd < 0.4 ? 'via' : 'pad';
          var rad = type === 'chip' ? 4 + Math.random() * 3 : type === 'via' ? 2 + Math.random() * 1.5 : 1.5 + Math.random();
          nodes.push({ x: nx + jx, y: ny + jy, radius: rad, type: type, pulsePhase: Math.random() * Math.PI * 2, pulseSpeed: 0.3 + Math.random() * 0.7 });
        }
      }
      for (var i = 0; i < nodes.length; i++) {
        var from = nodes[i];
        var conn = 1 + Math.floor(Math.random() * 2);
        var cands = [];
        for (var j = 0; j < nodes.length; j++) {
          if (i === j) continue;
          var to = nodes[j];
          var d = Math.sqrt((from.x - to.x) * (from.x - to.x) + (from.y - to.y) * (from.y - to.y));
          if (d < spacing * 2.5) cands.push({ idx: j, dist: d });
        }
        cands.sort(function (a, b) { return a.dist - b.dist; });
        for (var k = 0; k < Math.min(conn, cands.length); k++) {
          var ti = cands[k].idx;
          if (traces.some(function (t) { return (t.fromNode === i && t.toNode === ti) || (t.fromNode === ti && t.toNode === i); })) continue;
          var tn = nodes[ti];
          var segs;
          if (Math.random() < 0.5) segs = [{ x1: from.x, y1: from.y, x2: tn.x, y2: from.y }, { x1: tn.x, y1: from.y, x2: tn.x, y2: tn.y }];
          else segs = [{ x1: from.x, y1: from.y, x2: from.x, y2: tn.y }, { x1: from.x, y1: tn.y, x2: tn.x, y2: tn.y }];
          traces.push({ segments: segs, fromNode: i, toNode: ti });
        }
      }
    }
    function spawn() {
      if (signals.length >= MAX || !traces.length) return;
      signals.push({ traceIndex: Math.floor(Math.random() * traces.length), segmentIndex: 0, progress: 0, speed: 0.008 + Math.random() * 0.012 });
    }
    var seed = isMobile ? 3 : 6;
    for (var s = 0; s < seed; s++) signals.push({ traceIndex: 0, segmentIndex: 0, progress: Math.random(), speed: 0.008 + Math.random() * 0.012 });

    var resizeT;
    function resize() {
      var dpr = Math.min(window.devicePixelRatio || 1, isMobile ? 1.5 : 2);
      var rect = canvas.getBoundingClientRect();
      width = rect.width; height = rect.height;
      canvas.width = width * dpr; canvas.height = height * dpr;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      generate();
    }
    window.addEventListener('resize', function () { clearTimeout(resizeT); resizeT = setTimeout(resize, 150); });
    resize();

    var time = 0;
    function draw(dt) {
      time += dt * 0.001;
      ctx.clearRect(0, 0, width, height);
      var cx = width / 2, cy = height / 2;
      var rgba = function (a) { return 'rgba(' + color.r + ',' + color.g + ',' + color.b + ',' + a + ')'; };
      for (var i = 0; i < traces.length; i++) {
        var trace = traces[i];
        for (var s = 0; s < trace.segments.length; s++) {
          var seg = trace.segments[s];
          var mx = (seg.x1 + seg.x2) / 2, my = (seg.y1 + seg.y2) / 2;
          var fade = Math.min(1, Math.sqrt((mx - cx) * (mx - cx) + (my - cy) * (my - cy)) / (Math.min(width, height) * 0.28));
          ctx.beginPath(); ctx.moveTo(seg.x1, seg.y1); ctx.lineTo(seg.x2, seg.y2);
          ctx.strokeStyle = rgba(0.06 + 0.06 * fade); ctx.lineWidth = 1; ctx.stroke();
        }
      }
      for (var n = 0; n < nodes.length; n++) {
        var nd = nodes[n];
        var fadeN = Math.min(1, Math.sqrt((nd.x - cx) * (nd.x - cx) + (nd.y - cy) * (nd.y - cy)) / (Math.min(width, height) * 0.28));
        var pulse = 0.7 + 0.3 * Math.sin(time * nd.pulseSpeed + nd.pulsePhase);
        var alpha = (0.1 + 0.2 * fadeN) * pulse;
        if (nd.type === 'chip') {
          var sz = nd.radius * 2;
          ctx.beginPath();
          if (ctx.roundRect) ctx.roundRect(nd.x - sz, nd.y - sz * 0.6, sz * 2, sz * 1.2, 2);
          else ctx.rect(nd.x - sz, nd.y - sz * 0.6, sz * 2, sz * 1.2);
          ctx.fillStyle = rgba(alpha * 0.5); ctx.fill();
          ctx.strokeStyle = rgba(alpha); ctx.lineWidth = 1; ctx.stroke();
        } else {
          ctx.beginPath(); ctx.arc(nd.x, nd.y, nd.radius, 0, Math.PI * 2);
          ctx.fillStyle = rgba(alpha * (nd.type === 'via' ? 0.4 : 1)); ctx.fill();
          if (nd.type === 'via') { ctx.strokeStyle = rgba(alpha); ctx.lineWidth = 1; ctx.stroke(); }
        }
      }
      for (var k = signals.length - 1; k >= 0; k--) {
        var sig = signals[k];
        if (sig.traceIndex >= traces.length) { signals.splice(k, 1); continue; }
        var tr = traces[sig.traceIndex];
        var sg = tr.segments[sig.segmentIndex];
        if (!sg) { signals.splice(k, 1); continue; }
        sig.progress += sig.speed;
        if (sig.progress >= 1) {
          sig.progress = 0; sig.segmentIndex++;
          if (sig.segmentIndex >= tr.segments.length) { signals.splice(k, 1); continue; }
          continue;
        }
        var x = sg.x1 + (sg.x2 - sg.x1) * sig.progress;
        var y = sg.y1 + (sg.y2 - sg.y1) * sig.progress;
        var grd = ctx.createRadialGradient(x, y, 0, x, y, 10);
        grd.addColorStop(0, rgba(0.5)); grd.addColorStop(0.5, rgba(0.15)); grd.addColorStop(1, rgba(0));
        ctx.beginPath(); ctx.arc(x, y, 10, 0, Math.PI * 2); ctx.fillStyle = grd; ctx.fill();
        ctx.beginPath(); ctx.arc(x, y, 2, 0, Math.PI * 2); ctx.fillStyle = 'rgba(255,255,255,0.8)'; ctx.fill();
      }
      if (Math.random() < 0.02) spawn();
    }

    var visible = true;
    if (typeof IntersectionObserver === 'function') {
      var io = new IntersectionObserver(function (es) { visible = es[0].isIntersecting; }, { threshold: 0 });
      io.observe(canvas);
    }
    if (prefersReduced) { draw(16); return; }
    var last = 0;
    function loop(t) {
      var d = t - last; last = t;
      if (visible && d < 200) draw(d);
      requestAnimationFrame(loop);
    }
    requestAnimationFrame(loop);
  }
  function ready() {
    document.querySelectorAll('canvas[data-circuit-canvas]').forEach(function (canvas) {
      /* Per-canvas guard: under Astro's ClientRouter (showcase fidelity)
         this module executes once per session but ready() re-runs on
         every astro:page-load, so fresh canvases init and already-live
         ones are left alone. */
      if (canvas.hasAttribute('data-circuit-init')) return;
      canvas.setAttribute('data-circuit-init', '1');
      init(canvas);
    });
  }
  if (document.readyState !== 'loading') ready();
  else document.addEventListener('DOMContentLoaded', ready);
  /* Soft navigations swap the body without DOMContentLoaded; no-op on
     sites without the ClientRouter (event never fires). */
  document.addEventListener('astro:page-load', ready);
})();
