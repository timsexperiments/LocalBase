import "./icon-names.js";

// The Video studio's starting picture (TS studio chooseSource and cover): an
// image from a file or a recent result is scaled and center-cropped to
// fill exactly the model's frame (data-width × data-height), encoded as PNG,
// and put back in the file input, so the server receives the picture the
// model accepts.

const maxSourceBytes = 25 << 20;

function cover(image, width, height) {
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const context = canvas.getContext("2d");
  if (!context) throw new Error("This browser cannot prepare images.");
  const scale = Math.max(width / image.naturalWidth, height / image.naturalHeight);
  const cropWidth = width / scale;
  const cropHeight = height / scale;
  context.imageSmoothingQuality = "high";
  context.drawImage(image, (image.naturalWidth - cropWidth) / 2, (image.naturalHeight - cropHeight) / 2,
    cropWidth, cropHeight, 0, 0, width, height);
  return canvas;
}

const pngBlob = (canvas) => new Promise((resolve, reject) =>
  canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error("Could not encode the image."))), "image/png"));

function loadImage(file) {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(file);
    const image = new Image();
    image.onload = () => { URL.revokeObjectURL(url); resolve(image); };
    image.onerror = () => { URL.revokeObjectURL(url); reject(new Error("Could not read the image.")); };
    image.src = url;
  });
}

function syncChip(has) {
  const value = document.querySelector("[data-video-chip-value]");
  if (value) value.textContent = has ? "Picture" : "Words";
}

async function prepare(input, file) {
  const box = input.parentElement;
  const status = box.querySelector("[data-video-status]");
  const preview = box.querySelector("[data-video-preview]");
  const clear = box.querySelector("[data-video-clear]");
  const say = (text) => { if (status) status.textContent = text; };
  const none = () => {
    input.value = "";
    preview?.classList.add("hidden");
    if (clear) clear.hidden = true;
    syncChip(false);
  };
  if (!file) return none();
  if (!file.type.startsWith("image/")) {
    none();
    say("Choose a PNG, JPEG or WebP image.");
    return;
  }
  if (file.size > maxSourceBytes) {
    none();
    say("Choose an image smaller than 25 MiB.");
    return;
  }
  const width = Number(input.dataset.width), height = Number(input.dataset.height);
  say("Preparing…");
  try {
    const canvas = cover(await loadImage(file), width, height);
    const png = await pngBlob(canvas);
    const transfer = new DataTransfer();
    transfer.items.add(new File([png], "start.png", { type: "image/png" }));
    input.files = transfer.files;
    if (preview) {
      if (preview.src) URL.revokeObjectURL(preview.src);
      preview.src = URL.createObjectURL(png);
      preview.classList.remove("hidden");
    }
    if (clear) clear.hidden = false;
    syncChip(true);
    say("Ready: cropped to fill the frame.");
  } catch (cause) {
    none();
    say(cause instanceof Error ? cause.message : "Could not read the image.");
  }
}

document.addEventListener("change", (event) => {
  const input = event.target;
  if (input instanceof HTMLInputElement && input.matches("[data-video-source]")) prepare(input, input.files?.[0]);
});

document.addEventListener("click", async (event) => {
  const use = event.target.closest?.("[data-video-use]");
  const input = document.querySelector("[data-video-source]");
  if (use && input) {
    const status = input.parentElement.querySelector("[data-video-status]");
    try {
      const response = await fetch(use.dataset.videoUse);
      if (!response.ok) throw new Error("gone");
      const blob = await response.blob();
      await prepare(input, new File([blob], "recent.png", { type: blob.type }));
    } catch {
      if (status) status.textContent = "That picture has expired. Choose another.";
    }
    return;
  }
  if (event.target.closest?.("[data-video-clear]") && input) {
    await prepare(input, null);
    const status = input.parentElement.querySelector("[data-video-status]");
    if (status) status.textContent = "It is cropped to fill the frame.";
  }
});

// A model change redraws the options: the chip follows whether it takes a picture.
document.addEventListener("htmx:afterSettle", () => {
  const chip = document.querySelector("[data-video-chip]");
  if (!chip) return;
  const input = document.querySelector("[data-video-source]");
  chip.hidden = !input;
  syncChip(!!input?.files?.length);
});
