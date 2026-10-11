// Starts explorer.wasm with a download progress bar. A file of its own, not
// an inline script: the portal's Content-Security-Policy refuses those.
(async () => {
  const el = (id) => document.getElementById(id);
  const fail = (msg) => { el("err").textContent = "The explorer could not start: " + msg; };
  const progress = (frac) => {
    const track = el("ptrack");
    if (frac == null) { track.classList.add("indeterminate"); return; }
    track.classList.remove("indeterminate");
    el("pbar").style.width = Math.max(0, Math.min(1, frac)) * 100 + "%";
  };
  if (typeof WebAssembly !== "object") { fail("this browser has no WebAssembly."); return; }
  try {
    const resp = await fetch("explorer.wasm");
    if (!resp.ok) throw new Error("explorer.wasm: HTTP " + resp.status);
    // Content-Length is the compressed size when the portal sends the .gz;
    // the body arrives decompressed, so the bar is a guess then.
    const total = resp.headers.get("content-encoding") ? 0 : +resp.headers.get("content-length") || 0;
    let bytes;
    if (resp.body && resp.body.getReader) {
      const reader = resp.body.getReader(), chunks = [];
      let got = 0;
      el("plabel").textContent = "Downloading the explorer…";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(value); got += value.length;
        progress(total ? got / total : null);
      }
      bytes = new Uint8Array(got);
      let off = 0;
      for (const c of chunks) { bytes.set(c, off); off += c.length; }
    } else {
      bytes = new Uint8Array(await resp.arrayBuffer());
    }
    progress(1);
    const go = new Go();
    const { instance } = await WebAssembly.instantiate(bytes, go.importObject);
    el("loading").style.display = "none";
    el("screen").style.display = "block";
    go.run(instance);
  } catch (e) {
    fail(e && e.message ? e.message : String(e));
  }
})();
