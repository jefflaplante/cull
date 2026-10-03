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
  var idxs = secs.map(function(s){ return NUMS.indexOf(parseInt(s.getAttribute('data-n'), 10)); });
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
