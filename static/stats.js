// stats.js — Chart.js logic for Essence QC stats page.
// Reads window.__statsData injected by the server into the stats-content fragment.

(function () {
  'use strict';

  let statsChart = null;

  function darkMode() {
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches;
  }

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function initChart() {
    const snaps  = window.__statsData;
    const canvas = document.getElementById('stats-chart-canvas');
    if (!snaps || !snaps.length || !canvas) return;

    // Destroy previous instance if the fragment was re-swapped.
    if (statsChart) {
      statsChart.destroy();
      statsChart = null;
    }

    const textColor   = cssVar('--foreground')      || (darkMode() ? '#e4e4e7' : '#18181b');
    const gridColor   = cssVar('--border')          || (darkMode() ? '#3f3f46' : '#e4e4e7');
    const tooltipBg   = cssVar('--card')            || (darkMode() ? '#18181b' : '#ffffff');
    const tooltipText = cssVar('--card-foreground') || textColor;

    const labels = snaps.map(s => {
      const d = new Date(s.generatedAt);
      return d.toLocaleString('fr-CA', {
        month: 'short', day: 'numeric',
        hour: '2-digit', minute: '2-digit',
      });
    });

    const pt = snaps.length > 60 ? 0 : 3;
    const datasets = [
      {
        label: 'Régulier',
        data: snaps.map(s => s.regularAvg > 0 ? +s.regularAvg.toFixed(2) : null),
        borderColor: '#3b82f6', backgroundColor: 'rgba(59,130,246,0.1)',
        tension: 0.3, fill: false, pointRadius: pt,
      },
      {
        label: 'Super',
        data: snaps.map(s => s.superAvg > 0 ? +s.superAvg.toFixed(2) : null),
        borderColor: '#f59e0b', backgroundColor: 'rgba(245,158,11,0.1)',
        tension: 0.3, fill: false, pointRadius: pt,
      },
      {
        label: 'Diesel',
        data: snaps.map(s => s.dieselAvg > 0 ? +s.dieselAvg.toFixed(2) : null),
        borderColor: '#22c55e', backgroundColor: 'rgba(34,197,94,0.1)',
        tension: 0.3, fill: false, pointRadius: pt,
      },
    ];

    const chartOpts = {
      responsive: true,
      maintainAspectRatio: true,
      interaction: { mode: 'index', intersect: false },
      plugins: {
        legend: {
          position: 'top',
          labels: { color: textColor },
        },
        tooltip: {
          backgroundColor: tooltipBg,
          titleColor: tooltipText,
          bodyColor: tooltipText,
          borderColor: gridColor,
          borderWidth: 1,
          callbacks: {
            label: ctx => ctx.dataset.label + ': ' +
              (ctx.parsed.y != null ? ctx.parsed.y.toFixed(1) + '¢/L' : '—'),
          },
        },
      },
      scales: {
        x: {
          ticks: { maxTicksLimit: 10, maxRotation: 30, color: textColor },
          grid:  { color: gridColor },
        },
        y: {
          title: { display: true, text: '¢/L', color: textColor },
          ticks: { callback: v => v + '¢', color: textColor },
          grid:  { color: gridColor },
        },
      },
    };

    statsChart = new Chart(canvas, { type: 'line', data: { labels, datasets }, options: chartOpts });
  }

  // Called inline from stats-content.html after __statsData is set.
  window.__initStatsChart = initChart;

  // Re-render on dark-mode change.
  if (window.matchMedia) {
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
      if (statsChart) initChart();
    });
  }

  // Re-init after any htmx swap of #stats-content.
  document.body.addEventListener('htmx:afterSwap', function (evt) {
    if (evt.detail && evt.detail.target && evt.detail.target.id === 'stats-content') {
      // __statsData was updated inline by the swapped fragment; just init the chart.
      if (window.__statsData) initChart();
    }
  });
})();
