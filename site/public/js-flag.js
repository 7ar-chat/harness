document.documentElement.classList.add('js');
// If the scroll script never reports in (blocked, offline, error), show everything rather than leave blocks hidden.
setTimeout(function () {
  if (!document.documentElement.dataset.ready) document.documentElement.classList.remove('js');
}, 4000);
