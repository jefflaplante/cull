(function(){
  var reduce = window.matchMedia('(prefers-reduced-motion: reduce)');

  // Rangefinder headline. The ghost image starts out of register and is brought into
  // focus like turning the ring; moving the pointer across the hero throws it out
  // again, and it settles back. Reduced motion: in focus from the start.
  (function () {
    var rf = document.querySelector('.rf'), hero = document.querySelector('.hero');
    if (!rf) return;
    if (reduce.matches) { rf.classList.add('focused'); return; }
    var off = 0.32, target = 0, cur = off, settle = null;
    function em() { return parseFloat(getComputedStyle(rf).fontSize) || 80; }
    function frame() {
      cur += (target - cur) * 0.055;
      if (Math.abs(target - cur) < 0.0008) cur = target;
      rf.style.setProperty('--rf', (cur * em()).toFixed(2) + 'px');
      rf.classList.toggle('focused', Math.abs(cur) < 0.004);
      if (cur !== target) requestAnimationFrame(frame); else running = false;
    }
    var running = false;
    function go() { if (!running) { running = true; requestAnimationFrame(frame); } }
    rf.style.setProperty('--rf', (off * em()) + 'px');
    // Focus once the headline's font is in, so the visitor sees it happen.
    (document.fonts && document.fonts.ready ? document.fonts.ready : Promise.resolve())
      .then(function () { rf.style.setProperty('--rf', (off * em()) + 'px'); setTimeout(function () { target = 0; go(); }, 600); });
    if (hero && window.matchMedia('(pointer:fine)').matches) {
      hero.addEventListener('pointermove', function (e) {
        var r = hero.getBoundingClientRect();
        target = ((e.clientX - r.left) / r.width - 0.5) * 0.3;
        go();
        clearTimeout(settle);
        settle = setTimeout(function () { target = 0; go(); }, 700);
      });
      hero.addEventListener('pointerleave', function () { target = 0; go(); });
    }
  })();

  // Lens barrel. A 35 mm focusing ring as it reads on the lens: near distances to the
  // left, infinity and the units on the right; feet in yellow above, metres in white
  // below; marks spaced by 1/distance. The page turns it from 1.2 m at the top to
  // infinity at the end. The focusing index sits at the centre of the browser window,
  // with the depth-of-field scale beneath it: each aperture's marks sit where a 35 mm
  // lens's depth of field ends (circle of confusion 0.03 mm), its line flares out
  // below, and the aperture is engraved at the bottom edge.
  (function () {
    var svg = document.querySelector('.barrel .scales');
    if (!svg) return;
    var NS = 'http://www.w3.org/2000/svg', H = 84, S = 560, UMAX = 1 / 0.7;
    var white = '#ece6da';
    var yellow = getComputedStyle(document.documentElement).getPropertyValue('--feet').trim() || '#e9c03a';
    var still = reduce.matches, ring = null, CX = 0;
    function el(name, attrs, text) {
      var e = document.createElementNS(NS, name);
      for (var k in attrs) e.setAttribute(k, attrs[k]);
      if (text != null) e.textContent = text;
      return e;
    }
    function build() {
      var r = svg.getBoundingClientRect();
      if (!r.width) return false;
      var k = H / r.height, W = Math.round(r.width * k);
      CX = (window.innerWidth / 2 - r.left) * k; // the window's centre, in drawing units
      svg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
      while (svg.firstChild) svg.removeChild(svg.firstChild);
      ring = el('g', {});
      // x = -u·S: u in dioptres (1/metres), infinity at x = 0, nearer to the left
      var feet = [[2.5, '2.5'], [3, '3'], [4, '4'], [5, '5'], [7, '7'], [10, '10'], [15, '15'], [30, '30']];
      var metres = [[0.7, '0.7'], [0.8, '0.8'], [1, '1'], [1.2, '1.2'], [1.5, '1.5'], [2, '2'], [3, '3'], [5, '5']];
      function mark(u, y, label, colour) {
        var x = -u * S;
        ring.appendChild(el('line', { x1: x, x2: x, y1: y + 3, y2: y + 8, stroke: colour, 'stroke-width': 1.1 }));
        ring.appendChild(el('text', { x: x, y: y, fill: colour, 'font-size': 12.5, 'font-weight': 500, 'text-anchor': 'middle' }, label));
      }
      feet.forEach(function (f) { mark(1 / (f[0] * 0.3048), 14, f[1], yellow); });
      metres.forEach(function (m) { mark(1 / m[0], 35, m[1], white); });
      [[14, yellow, 'ft'], [35, white, 'm']].forEach(function (row) {
        ring.appendChild(el('text', { x: 0, y: row[0] + 1.5, fill: row[1], 'font-size': 16, 'text-anchor': 'middle' }, '∞'));
        ring.appendChild(el('text', { x: 24, y: row[0], fill: row[1], 'font-size': 10, 'letter-spacing': '0.12em', 'text-anchor': 'middle', opacity: 0.85 }, row[2]));
      });
      for (var u = 0.05; u < UMAX; u += 0.05) {
        ring.appendChild(el('line', { x1: -u * S, x2: -u * S, y1: 39, y2: 41, stroke: white, opacity: 0.3 }));
      }
      svg.appendChild(ring);
      // fixed: the focusing index and the depth-of-field scale beneath it
      svg.appendChild(el('line', { x1: CX, x2: CX, y1: 41, y2: 54, stroke: white, 'stroke-width': 1.4 }));
      var stops = [[16, '16'], [11, '11'], [8, '8'], [5.6, ''], [4, '4'], [2.8, '']];
      stops.forEach(function (st) {
        var off = st[0] * 0.03 / (35 * 35) * 1000 * S; // N·c/f² in dioptres, to px
        [-1, 1].forEach(function (side) {
          var x = CX + side * off, end = CX + side * off * 1.55;
          svg.appendChild(el('line', { x1: x, x2: x, y1: 46, y2: 53, stroke: white, 'stroke-width': 1, opacity: st[1] ? 0.95 : 0.45 }));
          // depth-of-field line, flaring out beneath its aperture to the bottom edge
          svg.appendChild(el('path', {
            d: 'M' + x + ' 54 Q ' + (x + side * off * 0.1) + ' 62 ' + end + ' 69',
            fill: 'none', stroke: white, 'stroke-width': 0.9, opacity: st[1] ? 0.4 : 0.18
          }));
          if (st[1]) svg.appendChild(el('text', { x: end, y: 81, fill: white, 'font-size': 11, 'text-anchor': 'middle', opacity: 0.92 }, st[1]));
        });
      });
      return true;
    }
    function turn() {
      if (!ring) return;
      var max = document.documentElement.scrollHeight - window.innerHeight;
      var p = still ? 0.6 : (max > 0 ? Math.min(1, window.scrollY / max) : 0);
      var u = (1 / 1.2) * (1 - p); // 1.2 m at the top of the page, infinity at the end
      ring.setAttribute('transform', 'translate(' + (CX + u * S).toFixed(2) + ' 0)');
    }
    function redraw() { if (build()) turn(); }
    redraw();
    window.addEventListener('resize', redraw);
    if (!still) {
      var queued = false;
      window.addEventListener('scroll', function () {
        if (!queued) { queued = true; requestAnimationFrame(function () { queued = false; turn(); }); }
      }, { passive: true });
    }
  })();

  // The phone pill tucks away while scrolling down (it would cover text), and comes
  // back on the way up, at the top and at the end of the page.
  (function () {
    var pill = document.querySelector('.pill'), last = window.scrollY;
    if (!pill) return;
    window.addEventListener('scroll', function () {
      var y = window.scrollY, end = document.documentElement.scrollHeight - window.innerHeight - 40;
      if (y < 80 || y > end || y < last - 6) pill.classList.remove('tucked');
      else if (y > last + 6) pill.classList.add('tucked');
      last = y;
    }, { passive: true });
  })();
  // The dial as engraved: a half-stop click between each full speed, then A (auto, in
  // red) and B (bulb) between 4000 and 1. Sections stop on full speeds, 1/30 to 1/4000.
  var FULL = [1,2,4,8,15,30,60,125,250,500,1000,2000,4000];
  var NUMS = [];
  FULL.forEach(function(n, i){ NUMS.push(n); if (i < FULL.length - 1) NUMS.push('half'); });
  NUMS.push('A', 'B');
  var STEP = 360 / NUMS.length;

  /* ---------- contact-sheet schematic: the recorded run ---------- */
  var run = [
    ['M1103817','c',2.5],['M1103813','k',8.7],['M1103823','k',8.6],['M1103821','r',8.5],
    ['M1103865','k',8.6],['M1103880','k',8.0],['M1103902','k',8.5],['M1103971','k',8.0],
    ['M1103979','k',8.3],['M1104110','r',4.5],['M1104112','k',8.0],['M1104114','k',8.0],
    ['M1104115','k',8.3],['M1104116','k',7.2],['M1104117','k',6.8],['M1104119','k',6.5],
    ['M1104118','k',7.0]
  ];
  var word = {k:'Keep', r:'Review', c:'Cull'};
  var tiles = document.getElementById('tiles');
  if (tiles) run.forEach(function(f, i){
    var li = document.createElement('li');
    li.className = 'tile ' + f[1] + (i === 2 ? ' sel' : '');
    li.style.setProperty('--x', (25 + (i * 37) % 55) + '%');
    li.style.setProperty('--y', (20 + (i * 53) % 60) + '%');
    li.style.setProperty('--g', (0.05 + ((i * 7) % 9) / 90).toFixed(3));
    li.innerHTML = '<span class="hd"><span class="fn">' + f[0].slice(-4) + '</span><span class="sc">' + f[2].toFixed(1) + '</span></span><span class="v eng">' + word[f[1]] + '</span>';
    tiles.appendChild(li);
  });

  /* ---------- terminal replays ----------
     Two real runs, recorded 2026-10-03 on 17 M11-P frames, played back when their
     terminal comes into view. The text is what cull printed, verbatim, except that
     local paths are shortened and each ranking reason is cut after its first
     sentence. Time is compressed; the "left" figures are the run's own. */
  var SCAN = [
    '[1/17] M1103817.DNG preview 9504x6320 (tiff-ifd) [no subject: no face; scan does not call a model]',
    '[2/17] M1103823.DNG preview 9504x6320 (tiff-ifd) [face q=287]',
    '[3/17] M1103821.DNG preview 9504x6320 (tiff-ifd) [no subject: no face; scan does not call a model]',
    '[4/17] M1103813.DNG preview 9504x6320 (tiff-ifd) [no subject: no face; scan does not call a model]',
    '[5/17] M1103865.DNG preview 9504x6320 (tiff-ifd) [face q=141]',
    '[6/17] M1103880.DNG preview 9504x6320 (tiff-ifd) [face q=155]',
    '[7/17] M1103902.DNG preview 9504x6320 (tiff-ifd) [no subject: no face; scan does not call a model]',
    '[8/17] M1103971.DNG preview 9504x6320 (tiff-ifd) [face q=124]',
    '[9/17] M1103979.DNG preview 9504x6320 (tiff-ifd) [face q=179]',
    '[10/17] M1104112.DNG preview 9504x6320 (tiff-ifd) [no subject: no face; scan does not call a model]',
    '[11/17] M1104110.DNG preview 9504x6320 (tiff-ifd) [no subject: no face; scan does not call a model]',
    '[12/17] M1104114.DNG preview 9504x6320 (tiff-ifd) [no subject: no face; scan does not call a model]',
    '[13/17] M1104115.DNG preview 9504x6320 (tiff-ifd) [no subject: no face; scan does not call a model]',
    '[14/17] M1104116.DNG preview 9504x6320 (tiff-ifd) [face q=105]',
    '[15/17] M1104117.DNG preview 9504x6320 (tiff-ifd) [face q=132]',
    '[16/17] M1104118.DNG preview 9504x6320 (tiff-ifd) [face q=188]',
    '[17/17] M1104119.DNG preview 9504x6320 (tiff-ifd) [face q=129]'
  ];
  var JUDGED = [
    '[1/17] M1103817.DNG CULL  sharp 2.5(missed_focus) exp 7.0(+0.3EV) comp 6.5(good)  [model: Woman\'s eyes with glasses]',
    '[2/17] M1103813.DNG KEEP  sharp 8.7(sharp) exp 8.0(+0.0EV) comp 7.5(good)  [model: Woman\'s near eye (glasses)]',
    '[3/17] M1103823.DNG KEEP  sharp 8.6(sharp) exp 6.5(+0.5EV) comp 6.5(croppable)  [face q=287]',
    '[4/17] M1103821.DNG REVIEW  sharp 8.5(sharp) exp 7.5(+0.1EV) comp 7.0(good)  [model: Smiling woman with glasses]',
    '[5/17] M1103865.DNG KEEP  sharp 8.6(sharp) exp 6.0(+1.0EV) comp 6.0(croppable)  [face q=141]',
    '[6/17] M1103880.DNG KEEP  sharp 8.0(sharp) exp 6.0(+1.3EV) comp 7.0(good)  [face q=155]',
    '[7/17] M1103902.DNG KEEP  sharp 8.5(sharp) exp 6.0(+1.0EV) comp 7.0(good)  [model: Woman\'s eyes with glasses]',
    '[8/17] M1103971.DNG KEEP  sharp 8.0(sharp) exp 6.5(+0.5EV) comp 7.0(good)  [face q=124]',
    '[9/17] M1103979.DNG KEEP  sharp 8.3(sharp) exp 6.5(+0.5EV) comp 6.0(croppable)  [face q=179]',
    '[10/17] M1104110.DNG REVIEW  sharp 4.5(soft) exp 6.5(+0.5EV) comp 6.0(croppable)  [model: Woman\'s glasses/eyes]',
    '[11/17] M1104112.DNG KEEP  sharp 8.0(sharp) exp 7.0(+0.4EV) comp 7.0(good)  [model: Woman\'s eyes with glasses]',
    '[12/17] M1104114.DNG KEEP  sharp 8.0(sharp) exp 6.5(+0.4EV) comp 6.5(good)  [model: Woman\'s eyes with glasses]',
    '[13/17] M1104115.DNG KEEP  sharp 8.3(sharp) exp 6.8(+0.4EV) comp 6.5(croppable)  [model: Woman\'s eyes behind glasses]',
    '[14/17] M1104116.DNG KEEP  sharp 7.2(acceptable) exp 6.5(+0.5EV) comp 6.5(good)  [face q=105]',
    '[15/17] M1104117.DNG KEEP  sharp 6.8(acceptable) exp 6.5(+0.5EV) comp 6.5(good)  [face q=132]',
    '[16/17] M1104119.DNG KEEP  sharp 6.5(acceptable) exp 5.5(+1.3EV) comp 6.5(good)  [face q=129]',
    '[17/17] M1104118.DNG KEEP  sharp 7.0(acceptable) exp 6.0(+1.0EV) comp 6.0(croppable)  [face q=188]'
  ];
  var SHOOT = 'Pictures/2025-12-28 Forest portraits';
  var REPLAYS = {
    offload: [
      ['cmd', 'cull offload LEICA_M Pictures --name "Forest portraits" --location "Forest Park, Portland"'],
      ['out', 'shoot folder: 2025-12-28 Forest portraits (date from earliest capture date, M1103813.DNG)'],
      ['out', '  into ' + SHOOT],
      ['out', '17 of 17 DNGs to copy, 1.1 GB (M1103813.DNG … M1104119.DNG)'],
      ['bytes', 'offload', 1.07e9, 2200, 2, 624],
      ['out', 'copied 17, skipped 0 (already there), failed 0: 1.1 GB in 2s (624 MB/s, verified)'],
      ['out', 'all 17 files verified on ' + SHOOT + ': safe to format the card', 'hi'],
      ['out', '17 DNGs found, 0 already done, 17 to process'],
      ['frames', 'scan', SCAN, 260, 0.78, false],
      ['out', 'report: ' + SHOOT + '/cull-report.json'],
      ['out', 'results: map[measured:17]'],
      ['out', 'next: cull judge --estimate "' + SHOOT + '"', 'dim']
    ],
    judge: [
      ['cmd', 'cull judge --backend claude-code --write-xmp "' + SHOOT + '"'],
      ['out', 'backend: claude-code, model: sonnet, auth: Claude subscription via ~/.local/bin/claude'],
      ['out', '17 DNGs found, 0 already done, 17 to process'],
      ['frames', 'judge', JUDGED, 420, 6.3, true],
      ['frames', 'rank', [
        'ranked set 1 (2 frames): M1104115.DNG wins — The two frames are almost identical: the same pose in a forest, with the subject looking back over her shoulder and smiling. …',
        'ranked set 2 (2 frames): M1104116.DNG wins — The two frames are nearly identical: the same pose in a forest, with a slight head tilt and a smile. …'
      ], 900, 4, false, 'sets'],
      ['out', 'report: ' + SHOOT + '/cull-report.json'],
      ['out', 'results: map[cull:1 keep:14 review:2]', 'hi'],
      ['out', 'tokens this run: in=153598 out=13840 (subscription, not billed per token)', 'dim']
    ]
  };

  function replayTerminal(box) {
    var script = REPLAYS[box.getAttribute('data-replay')];
    if (!script) return;
    var title = box.getAttribute('data-title') || 'cull';
    box.innerHTML = '';
    box.classList.add('live');
    var bar = document.createElement('div');
    bar.className = 'replay-bar';
    bar.innerHTML = '<span class="leds" aria-hidden="true"><i></i><i></i><i></i></span><span class="replay-title"></span>' +
      '<button class="replay-again eng" type="button" hidden>Replay</button>';
    bar.querySelector('.replay-title').textContent = title;
    var body = document.createElement('div');
    body.className = 'replay-body';
    body.setAttribute('role', 'log');
    body.setAttribute('aria-label', title + ', a recorded run');
    box.appendChild(bar); box.appendChild(body);
    var again = bar.querySelector('.replay-again');
    var timers = [], token = 0;

    function line(text, cls) {
      var d = document.createElement('div');
      d.className = 'rl' + (cls ? ' ' + cls : '');
      d.textContent = text;
      body.appendChild(d);
      body.scrollTop = body.scrollHeight;
      return d;
    }
    function gauge(label) {
      var d = document.createElement('div');
      d.className = 'rl rgauge';
      d.innerHTML = '<b></b><span class="rbar"><i></i></span><span class="rtxt"></span>';
      d.querySelector('b').textContent = label;
      body.appendChild(d);
      return {
        set: function (frac, text) {
          d.querySelector('i').style.width = (Math.max(0, Math.min(1, frac)) * 100).toFixed(1) + '%';
          d.querySelector('.rtxt').textContent = text;
          body.scrollTop = body.scrollHeight;
        }, el: d
      };
    }
    function left(s) {
      s = Math.max(0, Math.round(s));
      return (s >= 60 ? Math.floor(s / 60) + 'm' + (s % 60) + 's' : s + 's') + ' left';
    }
    function gb(b) { return (b / 1e9).toFixed(1) + ' GB'; }
    function wait(ms, fn) { var t = token; timers.push(setTimeout(function () { if (t === token) fn(); }, ms)); }

    // Each step calls next() when done; instant=true renders the final state at once.
    function run(instant) {
      token++; timers.forEach(clearTimeout); timers = [];
      body.innerHTML = ''; again.hidden = true;
      var i = 0;
      function next() {
        if (i >= script.length) { again.hidden = instant; return; }
        var s = script[i++];
        if (s[0] === 'cmd') {
          var d = line('', 'cmd');
          if (instant) { d.textContent = s[1]; return next(); }
          var k = 0;
          (function type() {
            d.textContent = s[1].slice(0, ++k);
            if (k < s[1].length) wait(18, type); else wait(380, next);
          })();
        } else if (s[0] === 'out') {
          line(s[1], s[2]);
          instant ? next() : wait(140, next);
        } else if (s[0] === 'bytes') {
          var g = gauge(s[1]), total = s[2], ms = s[3], real = s[4], mbs = s[5];
          if (instant) { g.set(1, gb(total) + '/' + gb(total) + '  done'); return next(); }
          var t0 = performance.now(), tk = token;
          (function tick() {
            if (tk !== token) return;
            var p = Math.min(1, (performance.now() - t0) / ms);
            g.set(p, p < 1 ? gb(total * p) + '/' + gb(total) + '  ' + mbs + ' MB/s, ' + left(real * (1 - p)) : gb(total) + '/' + gb(total) + '  done');
            if (p < 1) requestAnimationFrame(tick); else wait(260, next);
          })();
        } else if (s[0] === 'frames') {
          var label = s[1], items = s[2], per = s[3], realPer = s[4], tally = s[5], unit = s[6] || 'frames';
          var n = items.length, done = 0, counts = { keep: 0, review: 0, cull: 0 };
          var gg = null, tl = null;
          function view() {
            if (gg) { body.removeChild(gg.el); }
            if (tl) { body.removeChild(tl); }
            gg = gauge(label);
            gg.set(done / n, done + '/' + n + ' ' + unit + '  ' + (done < n ? left(realPer * (n - done)) : 'done'));
            if (tally) {
              var parts = [];
              ['keep', 'review', 'cull'].forEach(function (k2) { if (counts[k2]) parts.push(k2 + ' ' + counts[k2]); });
              tl = line(parts.join(' · '), 'rtally');
            }
          }
          function one() {
            var text = items[done++];
            var m = / (KEEP|REVIEW|CULL) /.exec(text);
            if (m) counts[m[1].toLowerCase()]++;
            if (gg) { body.removeChild(gg.el); gg = null; }
            if (tl) { body.removeChild(tl); tl = null; }
            line(text, m ? m[1].toLowerCase() : '');
            view();
          }
          if (instant) { while (done < n) one(); return next(); }
          view();
          (function step() { one(); if (done < n) wait(per, step); else wait(400, next); })();
        }
      }
      next();
    }
    again.addEventListener('click', function () { run(false); });
    if (reduce.matches || !('IntersectionObserver' in window)) { run(true); return; }
    run(true); // the final state until it comes into view, so nothing jumps
    var seen = false;
    new IntersectionObserver(function (es, ob) {
      es.forEach(function (e) {
        if (e.isIntersecting && !seen) { seen = true; ob.disconnect(); run(false); }
      });
    }, { threshold: 0.35 }).observe(box);
  }
  document.querySelectorAll('[data-replay]').forEach(replayTerminal);

  /* ---------- copy buttons ---------- */
  document.querySelectorAll('[data-copy]').forEach(function(btn){
    btn.addEventListener('click', function(){
      var pre = document.getElementById(btn.getAttribute('data-copy'));
      var text = pre.textContent.replace(/^\$\s*/, '').trim();
      var done = function(){ btn.textContent = 'Copied'; setTimeout(function(){ btn.textContent = 'Copy'; }, 1600); };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function(){ fallback(text); done(); });
      } else { fallback(text); done(); }
    });
  });
  function fallback(text){
    var ta = document.createElement('textarea');
    ta.value = text; ta.setAttribute('readonly',''); ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); } catch(e) {}
    document.body.removeChild(ta);
  }

  /* ---------- reveals, schematic, counters ---------- */
  function countUp(el){
    var end = parseFloat(el.getAttribute('data-count'));
    var dec = parseInt(el.getAttribute('data-dec') || '0', 10);
    if (reduce.matches) { el.textContent = end.toFixed(dec); return; }
    var t0 = null, dur = 1300;
    function step(t){
      if (!t0) t0 = t;
      var p = Math.min(1, (t - t0) / dur);
      var e = 1 - Math.pow(1 - p, 3);
      el.textContent = (end * e).toFixed(dec);
      if (p < 1) requestAnimationFrame(step);
    }
    el.textContent = (0).toFixed(dec);
    requestAnimationFrame(step);
  }
  var targets = document.querySelectorAll('.reveal, .schem, .diagram, .meter, [data-count]');
  if ('IntersectionObserver' in window) {
    var io = new IntersectionObserver(function(entries){
      entries.forEach(function(en){
        if (!en.isIntersecting) return;
        var el = en.target;
        if (el.hasAttribute('data-count')) countUp(el);
        el.classList.add('in');
        io.unobserve(el);
      });
    }, {rootMargin:'0px 0px -12% 0px', threshold:0.12});
    targets.forEach(function(el){ io.observe(el); });
  } else {
    targets.forEach(function(el){ el.classList.add('in'); });
  }

  /* ---------- the dial ---------- */
  var secs = Array.prototype.slice.call(document.querySelectorAll('main > section[data-n]'));
  var idxs = secs.map(function(s){ var v = s.getAttribute('data-n'), n = parseInt(v, 10); return NUMS.indexOf(isNaN(n) ? v : n); });
  var dial = document.getElementById('dial');
  var numsBox = document.getElementById('dialNums');
  var cap = document.getElementById('dialCap');
  var dot = document.getElementById('dialIndex');
  var pillKnurl = document.getElementById('pillKnurl');
  var pillNum = document.getElementById('pillNum');
  var pillName = document.getElementById('pillName');
  var pillMenu = document.getElementById('pillMenu');
  var pillBtn = document.getElementById('pillBtn');
  var navLinks = document.querySelectorAll('[data-nav]');

  var dialLinks = {};
  NUMS.forEach(function(n, i){
    var k = idxs.indexOf(i), el;
    if (k > -1) {
      var s = secs[k];
      el = document.createElement('a');
      el.href = '#' + s.id;
      el.setAttribute('aria-label', s.getAttribute('data-name') + ' (' + n + ')');
      dialLinks[k] = el;
    } else {
      el = document.createElement('span');
      el.setAttribute('aria-hidden', 'true');
    }
    if (n === 'half') { el.className = 'half'; el.textContent = ''; }
    else { el.textContent = n; if (n === 'A') el.className = 'auto'; }
    el.style.setProperty('--a', (i * STEP) + 'deg');
    numsBox.appendChild(el);
  });

  var menuLinks = [];
  secs.forEach(function(s, k){
    var li = document.createElement('li');
    var a = document.createElement('a');
    a.href = '#' + s.id;
    a.innerHTML = '<span>' + s.getAttribute('data-n') + '</span><span>' + s.getAttribute('data-name') + '</span>';
    a.className = 'eng';
    a.addEventListener('click', function(){ closeMenu(false); });
    li.appendChild(a); pillMenu.appendChild(li); menuLinks.push(a);
  });
  function closeMenu(focus){
    pillMenu.hidden = true; pillBtn.setAttribute('aria-expanded', 'false');
    if (focus) pillBtn.focus();
  }
  pillBtn.addEventListener('click', function(){
    var open = pillMenu.hidden;
    pillMenu.hidden = !open; pillBtn.setAttribute('aria-expanded', String(open));
    if (open && menuLinks[active]) menuLinks[active].focus();
  });
  document.addEventListener('keydown', function(e){ if (e.key === 'Escape' && !pillMenu.hidden) closeMenu(true); });
  document.addEventListener('click', function(e){ if (!pillMenu.hidden && !document.getElementById('pill').contains(e.target)) closeMenu(false); });

  var schem = document.getElementById('schem');
  var rail = schem && schem.querySelector('.schem-rail'), dots = schem && schem.querySelector('.schem-dots');
  function placeRail(){
    if (!schem) return;
    var nodes = schem.querySelectorAll('.node');
    var r0 = schem.getBoundingClientRect(), a = nodes[0].getBoundingClientRect(), b = nodes[nodes.length - 1].getBoundingClientRect();
    var x1 = a.left + a.width / 2 - r0.left, y1 = a.top + a.height / 2 - r0.top;
    var x2 = b.left + b.width / 2 - r0.left, y2 = b.top + b.height / 2 - r0.top;
    [rail, dots].forEach(function(el){
      el.style.left = (x1 - (x2 - x1 < 2 ? 0.5 : 0)) + 'px';
      el.style.top = y1 + 'px';
      el.style.width = Math.max(1, x2 - x1) + 'px';
      el.style.height = Math.max(1, y2 - y1) + 'px';
    });
  }
  var tops = [];
  function measure(){
    placeRail();
    tops = secs.map(function(s){ return s.getBoundingClientRect().top + window.scrollY; });
  }
  function smooth(x){ x = Math.max(0, Math.min(1, x)); return x * x * (3 - 2 * x); }

  var target = idxs[0], cur = idxs[0], active = 0, raf = 0;
  function compute(){
    var probe = window.scrollY + window.innerHeight * 0.38;
    var a = 0;
    for (var k = 0; k < tops.length; k++) if (probe >= tops[k]) a = k;
    var pos = idxs[a], act = a;
    if (a < secs.length - 1) {
      var frac = Math.max(0, Math.min(1, (probe - tops[a]) / (tops[a + 1] - tops[a])));
      /* detent: creep slightly while inside a section, turn to the next numeral near its edge */
      var f = frac < 0.72 ? frac * 0.1 : 0.072 + 0.928 * smooth((frac - 0.72) / 0.28);
      if (reduce.matches) f = 0;
      pos = idxs[a] + f * (idxs[a + 1] - idxs[a]);
      if (f > 0.5) act = a + 1;
    }
    if (window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 4) {
      act = secs.length - 1; pos = idxs[act];
    }
    target = pos;
    if (act !== active) setActive(act);
  }
  function setActive(k){
    active = k;
    var s = secs[k], n = s.getAttribute('data-n'), name = s.getAttribute('data-name');
    cap.innerHTML = '<b>' + n + '</b> · ' + name;
    pillNum.textContent = n; pillName.textContent = name;
    Object.keys(dialLinks).forEach(function(i){ dialLinks[i].setAttribute('aria-current', String(+i === k)); });
    menuLinks.forEach(function(a, i){ a.setAttribute('aria-current', String(i === k)); });
    navLinks.forEach(function(a){ a.setAttribute('aria-current', String(a.getAttribute('data-nav') === s.id)); });
    if (!reduce.matches) { dot.classList.add('tick'); setTimeout(function(){ dot.classList.remove('tick'); }, 220); }
  }
  function render(){
    raf = 0;
    if (reduce.matches) cur = target;
    else cur += (target - cur) * 0.14;
    if (Math.abs(target - cur) < 0.0005) cur = target;
    var deg = (-cur * STEP).toFixed(3) + 'deg';
    dial.style.setProperty('--rot', deg);
    pillKnurl.style.transform = 'rotate(' + deg + ')';
    if (cur !== target) raf = requestAnimationFrame(render);
  }
  function onScroll(){ compute(); if (!raf) raf = requestAnimationFrame(render); }

  measure(); compute(); setActive(active); cur = target; render();
  window.addEventListener('scroll', onScroll, {passive:true});
  window.addEventListener('resize', function(){ measure(); onScroll(); });
  window.addEventListener('load', function(){ measure(); onScroll(); });
  if ('ResizeObserver' in window) new ResizeObserver(function(){ measure(); onScroll(); }).observe(document.body);
})();
