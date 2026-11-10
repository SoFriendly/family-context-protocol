const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
if ('IntersectionObserver' in window && !reducedMotion.matches) {
  const observer = new IntersectionObserver((entries) => {
    for (const entry of entries) {
      if (!entry.isIntersecting) continue;
      entry.target.classList.add('reveal');
      observer.unobserve(entry.target);
    }
  }, { threshold: 0.15 });
  document.querySelectorAll('.comparison-state, .comparison-message, .outcome, .assistants h2, .assistants p, .assistant-logos a, .facts p, .protocol h2, .protocol-copy p, .api, .privacy h2, .privacy p, .privacy li, .setup h2, .setup-heading p, .downloads a, details, footer > div').forEach((element) => observer.observe(element));
}
