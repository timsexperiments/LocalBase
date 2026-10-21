// The Lab's starting picture for image-to-video models (TS studio
// chooseSource and cover): the chosen image is scaled and center-cropped to
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

async function prepare(input) {
  const box = input.parentElement;
  const status = box.querySelector("[data-video-status]");
  const preview = box.querySelector("[data-video-preview]");
  const say = (text) => { if (status) status.textContent = text; };
  const file = input.files?.[0];
  if (!file) {
    preview?.classList.add("hidden");
    return;
  }
  if (!file.type.startsWith("image/")) {
    input.value = "";
    say("Choose a PNG, JPEG or WebP image.");
    return;
  }
  if (file.size > maxSourceBytes) {
    input.value = "";
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
    say(`Ready: cropped to ${width}×${height}.`);
  } catch (cause) {
    input.value = "";
    preview?.classList.add("hidden");
    say(cause instanceof Error ? cause.message : "Could not read the image.");
  }
}

document.addEventListener("change", (event) => {
  const input = event.target;
  if (input instanceof HTMLInputElement && input.matches("[data-video-source]")) prepare(input);
});
