// map.js — Leaflet map logic for Essence QC
// Reads window.__stations and window.__deltas injected by the server.

(function () {
  'use strict';

  let currentFuel   = 'regular';
  let currentRegion = '';
  let clusterMode   = 'avg'; // 'min' | 'avg' | 'max'
  let allStations   = window.__stations || [];
  let allMarkers    = [];
  let visibleSet    = new Set();
  let minPrice = 0, maxPrice = 300;
  let sliderTimer   = null;
  const stationDeltas = window.__deltas || {};

  // ── Map setup (deferred to init section below) ─────────────
  let map;
  let clusterGroup;

  // ── Colour helpers ─────────────────────────────────────────
  function priceColor(price, min, max) {
    const t = Math.max(0, Math.min(1, (price - min) / (max - min)));
    const colors = [
      [22, 163, 74], [101, 163, 13], [202, 138, 4], [234, 88, 12], [220, 38, 38],
    ];
    const idx  = Math.min(Math.floor(t * (colors.length - 1)), colors.length - 2);
    const frac = t * (colors.length - 1) - idx;
    const c0 = colors[idx], c1 = colors[idx + 1];
    return `rgb(${Math.round(c0[0] + (c1[0] - c0[0]) * frac)},${Math.round(c0[1] + (c1[1] - c0[1]) * frac)},${Math.round(c0[2] + (c1[2] - c0[2]) * frac)})`;
  }

  function circleSvg(fill, label) {
    return `<svg class="pin-icon" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 36 36" width="36" height="36">
      <circle cx="18" cy="18" r="17" fill="${fill}" stroke="#fff" stroke-width="2"/>
      <text x="18" y="22" text-anchor="middle" fill="#fff" font-size="10" font-weight="700" font-family="sans-serif">${label}</text>
    </svg>`;
  }

  // ── Brand colours ──────────────────────────────────────────
  const BRAND_COLORS = {
    'ultramar':     '#0057a8',
    'petro-canada': '#e4002b',
    'esso':         '#003087',
    'shell':        '#dd1d21',
    'pioneer':      '#f47920',
    'couche-tard':  '#c8102e',
    'circle k':     '#c8102e',
    'crevier':      '#e3000f',
    'irving':       '#006747',
    'gilbert':      '#00796b',
    'sonic':        '#7b2d8b',
    'dépanneur':    '#475569',
    'indépendant':  '#334155',
  };

  const FALLBACK_PALETTE = [
    '#b45309', '#0369a1', '#065f46', '#7c3aed',
    '#be185d', '#0f766e', '#b91c1c', '#1d4ed8',
    '#15803d', '#a21caf', '#c2410c', '#0e7490',
  ];

  function brandColor(brand) {
    const key = (brand || '').toLowerCase().trim();
    if (BRAND_COLORS[key]) return BRAND_COLORS[key];
    let h = 0;
    for (let i = 0; i < key.length; i++) h = (h * 31 + key.charCodeAt(i)) & 0xffff;
    return FALLBACK_PALETTE[h % FALLBACK_PALETTE.length];
  }

  function luminance(hex) {
    const r   = parseInt(hex.slice(1, 3), 16) / 255;
    const g   = parseInt(hex.slice(3, 5), 16) / 255;
    const b   = parseInt(hex.slice(5, 7), 16) / 255;
    const lin = c => c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
    return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
  }

  function contrastFg(bgHex) {
    const L       = luminance(bgHex);
    const onWhite = (L + 0.05) / 0.05;
    const onBlack = 1.05 / (L + 0.05);
    return onBlack > onWhite ? '#ffffff' : '#000000';
  }

  function brandBadgeHtml(brand) {
    if (!brand) return '';
    const bg = brandColor(brand);
    const fg = contrastFg(bg);
    return `<span class="badge" style="--primary:${bg};--primary-foreground:${fg}">${brand}</span>`;
  }

  function priceDeltaHtml(pct, elapsedHours) {
    if (pct == null) return '';
    if (Math.abs(pct) < 0.05) return '';
    const up    = pct > 0;
    const color = up ? '#dc2626' : '#16a34a';
    const arrow = up
      ? '<svg width="10" height="10" viewBox="0 0 10 10" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="2,8 5,2 8,8"/></svg>'
      : '<svg width="10" height="10" viewBox="0 0 10 10" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="2,2 5,8 8,2"/></svg>';
    const sign  = up ? '+' : '';
    let timeLabel = '';
    if (elapsedHours != null) {
      if (elapsedHours < 1) timeLabel = ' (< 1h)';
      else timeLabel = ` (${Math.min(24, Math.round(elapsedHours))}h)`;
    }
    return ` <span style="color:${color};font-size:11px;white-space:nowrap;">${arrow}${sign}${pct.toFixed(2)}%${timeLabel}</span>`;
  }

  // ── Marker building ────────────────────────────────────────
  function rebuildMarkers(fitMap) {
    clusterGroup.clearLayers();
    visibleSet.clear();

    const scopedStations = currentRegion
      ? allStations.filter(s => s.region === currentRegion)
      : allStations;
    const prices = scopedStations.map(s => s[currentFuel]).filter(p => p > 0);
    minPrice = prices.length ? Math.min(...prices) : 0;
    maxPrice = prices.length ? Math.max(...prices) : 300;

    const slider = document.getElementById('price-slider');
    slider.min   = Math.floor(minPrice);
    slider.max   = Math.ceil(maxPrice);
    slider.value = Math.ceil(maxPrice);
    document.getElementById('slider-min').textContent   = Math.floor(minPrice) + '¢';
    document.getElementById('slider-max').textContent   = Math.ceil(maxPrice) + '¢';
    document.getElementById('slider-label').textContent = Math.ceil(maxPrice) + '¢/L';

    const fuelLabel = { regular: 'Régulier', super: 'Super', diesel: 'Diesel' }[currentFuel];
    document.getElementById('legend-title').textContent = `Prix ${fuelLabel.toLowerCase()} (¢/L)`;
    document.getElementById('min-price').textContent    = minPrice.toFixed(1) + '¢';
    document.getElementById('max-price').textContent    = maxPrice.toFixed(1) + '¢';

    const GREY = '#9ca3af';
    allMarkers = allStations.map(s => {
      const price = s[currentFuel];
      const fill  = price > 0 ? priceColor(price, minPrice, maxPrice) : GREY;
      const label = price > 0 ? price.toFixed(1) : '—';
      const icon  = L.divIcon({
        html: circleSvg(fill, label), className: '',
        iconSize: [36, 36], iconAnchor: [18, 18], popupAnchor: [0, -20],
      });
      const marker  = L.marker([s.lat, s.lng], { icon });
      marker.fuelPrices = { regular: s.regular, super: s.super, diesel: s.diesel };
      const mapsUrl = `https://www.google.com/maps/dir/?api=1&destination=${encodeURIComponent(s.address + ', Québec')}`;
      const d       = stationDeltas[s.address] || {};
      const eh      = d.elapsedHours != null ? d.elapsedHours : null;
      let popup     = `<strong>${s.name}</strong><br>${brandBadgeHtml(s.brand)}<br><a href="${mapsUrl}" target="_blank" rel="noopener">${s.address}</a><br>`;
      popup += `<br><strong>Régulier:</strong> ${s.regular.toFixed(1)}¢/L${priceDeltaHtml(d.regular, eh)}`;
      if (s.super  > 0) popup += `<br><strong>Super:</strong> ${s.super.toFixed(1)}¢/L${priceDeltaHtml(d.super, eh)}`;
      if (s.diesel > 0) popup += `<br><strong>Diesel:</strong> ${s.diesel.toFixed(1)}¢/L${priceDeltaHtml(d.diesel, eh)}`;
      marker.bindPopup(popup);
      return marker;
    });

    applyPriceFilter(parseFloat(slider.value), fitMap);
  }

  function applyPriceFilter(priceMax, fitMap) {
    const toAdd    = [];
    const toRemove = [];

    allStations.forEach((s, i) => {
      const price    = s[currentFuel];
      const inRegion = !currentRegion || s.region === currentRegion;
      const visible  = inRegion && (price <= 0 || price <= priceMax);
      if (visible && !visibleSet.has(i)) {
        toAdd.push(allMarkers[i]);
        visibleSet.add(i);
      } else if (!visible && visibleSet.has(i)) {
        toRemove.push(allMarkers[i]);
        visibleSet.delete(i);
      }
    });

    if (toRemove.length) clusterGroup.removeLayers(toRemove);
    if (toAdd.length)    clusterGroup.addLayers(toAdd);

    document.getElementById('visible-count').textContent =
      visibleSet.size + ' / ' + allStations.length + ' stations';

    if (fitMap && currentRegion) {
      const latlngs = allStations
        .filter(s => s.region === currentRegion)
        .map(s => [s.lat, s.lng]);
      if (latlngs.length > 0) {
        map.fitBounds(L.latLngBounds(latlngs), { padding: [40, 40], maxZoom: 13 });
      }
    }
  }

  // ── Wire up controls ───────────────────────────────────────
  function bindControls() {
    document.getElementById('region-select').addEventListener('change', function () {
      currentRegion = this.value;
      rebuildMarkers(true);
    });

    document.getElementById('price-slider').addEventListener('input', function () {
      const val = parseFloat(this.value);
      document.getElementById('slider-label').textContent = val.toFixed(1) + '¢/L';
      clearTimeout(sliderTimer);
      sliderTimer = setTimeout(() => applyPriceFilter(val), 80);
    });

    document.querySelectorAll('.cluster-btn').forEach(btn => {
      btn.addEventListener('click', () => {
        if (btn.dataset.mode === clusterMode) return;
        clusterMode = btn.dataset.mode;
        document.querySelectorAll('.cluster-btn').forEach(b =>
          b.classList.toggle('active', b.dataset.mode === clusterMode));
        if (allStations.length) clusterGroup.refreshClusters();
      });
    });

    document.querySelectorAll('.fuel-btn[data-fuel]').forEach(btn => {
      btn.addEventListener('click', () => {
        if (btn.dataset.fuel === currentFuel) return;
        currentFuel = btn.dataset.fuel;
        document.querySelectorAll('.fuel-btn[data-fuel]').forEach(b =>
          b.classList.toggle('active', b.dataset.fuel === currentFuel));
        rebuildMarkers();
      });
    });
  }

  // ── Initialise ─────────────────────────────────────────────
  const loadingEl = document.getElementById('loading');
  try {
    // Create map and cluster group now that the DOM is ready.
    map = L.map('map', { zoomControl: false }).setView([46.8, -71.2], 7);
    L.control.zoom({ position: 'topright' }).addTo(map);
    L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
      attribution: '&copy; <a href="https://openstreetmap.org/copyright">OpenStreetMap</a> | Données: <a href="https://regieessencequebec.ca">Régie de l\'énergie du Québec</a>',
      maxZoom: 18,
    }).addTo(map);

    clusterGroup = L.markerClusterGroup({
      maxClusterRadius: 50,
      spiderfyOnMaxZoom: true,
      showCoverageOnHover: false,
      zoomToBoundsOnClick: true,
      disableClusteringAtZoom: 14,
      iconCreateFunction: function (cluster) {
        const children = cluster.getAllChildMarkers();
        const prices   = children.map(m => m.fuelPrices[currentFuel]).filter(p => p > 0);
        const count    = cluster.getChildCount();
        let fill, label;
        if (prices.length > 0) {
          let displayPrice;
          if (clusterMode === 'min')      displayPrice = Math.min(...prices);
          else if (clusterMode === 'max') displayPrice = Math.max(...prices);
          else                            displayPrice = prices.reduce((a, b) => a + b, 0) / prices.length;
          fill  = priceColor(displayPrice, minPrice, maxPrice);
          label = displayPrice.toFixed(1);
        } else {
          fill  = '#9ca3af';
          label = count;
        }
        const size = count < 10 ? 44 : count < 50 ? 52 : 60;
        const r    = size / 2;
        const fs   = count < 10 ? 11 : count < 50 ? 10 : 9;
        const svg  = `<svg class="pin-icon" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${size} ${size}" width="${size}" height="${size}">
          <circle cx="${r}" cy="${r}" r="${r - 2}" fill="${fill}" stroke="#fff" stroke-width="2.5"/>
          <text x="${r}" y="${r + fs * 0.38}" text-anchor="middle" fill="#fff" font-size="${fs}" font-weight="700" font-family="sans-serif">${label}</text>
        </svg>`;
        return L.divIcon({ html: svg, className: '', iconSize: [size, size], iconAnchor: [r, r] });
      },
    });

    loadingEl.style.display = 'none';
    document.getElementById('map-stats').textContent = allStations.length + ' stations';

    rebuildMarkers();
    map.addLayer(clusterGroup);

    bindControls();

    // Expose for use after htmx page-swap back to the map page.
    window.__mapInvalidate = function () {
      setTimeout(() => map.invalidateSize(), 50);
    };
  } catch (err) {
    loadingEl.innerHTML = '<span style="color:#dc2626;padding:20px;text-align:center;">Erreur JavaScript: ' + err.message + '</span>';
  }
})();
