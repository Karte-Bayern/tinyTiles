/* global Go */
(function () {
  "use strict";

  const status = document.getElementById("status");
  const value = (id) => document.getElementById(id).value;
  const integer = (id) => Number.parseInt(value(id), 10);
  const write = (message) => { status.textContent = typeof message === "string" ? message : JSON.stringify(message, null, 2); };

  async function instantiate() {
    const go = new Go();
    let instance;
    try {
      const result = await WebAssembly.instantiateStreaming(fetch("tinytiles.wasm"), go.importObject);
      instance = result.instance;
    } catch (_) {
      const bytes = await (await fetch("tinytiles.wasm")).arrayBuffer();
      instance = (await WebAssembly.instantiate(bytes, go.importObject)).instance;
    }
    // Go's runtime deliberately remains alive to receive Promise callbacks.
    go.run(instance);
    for (let attempt = 0; attempt < 200 && !window.tinyTiles; attempt += 1) {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    if (!window.tinyTiles) throw new Error("tinyTiles WASM API did not initialize");
    write({ ready: true, version: window.tinyTiles.version, message: "Open an IndexedDB cache, then synchronize a range." });
  }

  function request() {
    return {
      dataset: value("dataset").trim() || undefined,
      ranges: [{
        z: integer("z"),
        x_min: integer("xmin"),
        x_max: integer("xmax"),
        y_min: integer("ymin"),
        y_max: integer("ymax")
      }],
      concurrency: integer("concurrency"),
      prune_previous: false
    };
  }

  document.getElementById("open").addEventListener("click", async () => {
    try { write(await window.tinyTiles.open(value("cache-name"))); } catch (error) { write({ error: String(error) }); }
  });
  document.getElementById("close").addEventListener("click", async () => {
    try { write(await window.tinyTiles.close()); } catch (error) { write({ error: String(error) }); }
  });
  document.getElementById("sync").addEventListener("click", async () => {
    try { write(await window.tinyTiles.sync(value("manifest-url"), request())); } catch (error) { write({ error: String(error) }); }
  });
  document.getElementById("get").addEventListener("click", async () => {
    try {
      const tile = await window.tinyTiles.get(value("dataset"), integer("z"), integer("xmin"), integer("ymin"));
      write({ found: tile.found, revision: tile.revision, bytes: tile.data ? tile.data.byteLength : 0, contentType: tile.contentType, checksum: tile.checksum });
    } catch (error) { write({ error: String(error) }); }
  });

  instantiate().catch((error) => write({ error: String(error) }));
}());
