document.body.addEventListener("htmx:xhr:progress", function (evt) {
  var bar = document.getElementById("upload-progress");
  if (!bar) return;
  if (evt.detail.total > 0) {
    bar.hidden = false;
    bar.value = (evt.detail.loaded / evt.detail.total) * 100;
  }
});
document.body.addEventListener("htmx:afterRequest", function (evt) {
  var bar = document.getElementById("upload-progress");
  if (bar && evt.detail.elt && evt.detail.elt.classList.contains("upload")) {
    bar.hidden = true;
    bar.value = 0;
  }
});
