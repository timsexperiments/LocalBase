/**
 * Static index of the catalog data files.
 *
 * Bun has no glob imports, so every model file is imported explicitly here.
 * The array order is the catalog order (it breaks recommendation ties), so
 * append new models where they belong rather than sorting this file.
 * `src/catalog/models/index.test.ts` fails when a data file is missing here.
 */
import llm_qwen3_5_4b_q4_k_m from "./llm/qwen3.5-4b-q4_k_m.json";
import llm_qwen3_5_9b_q4_k_m from "./llm/qwen3.5-9b-q4_k_m.json";
import llm_gpt_oss_20b_q4_k_m from "./llm/gpt-oss-20b-q4_k_m.json";
import llm_qwen3_8_27b_q4_k_m from "./llm/qwen3.8-27b-q4_k_m.json";
import llm_qwen3_embedding_0_6b_q8_0 from "./llm/qwen3-embedding-0.6b-q8_0.json";
import llm_qwen3_embedding_4b_q4_k_m from "./llm/qwen3-embedding-4b-q4_k_m.json";
import llm_clef_flash_q4_k_m from "./llm/clef-flash-q4_k_m.json";
import llm_clef_q4_k_m from "./llm/clef-q4_k_m.json";
import stt_whisper_base_q8_0 from "./stt/whisper-base-q8_0.json";
import stt_whisper_large_v3_turbo from "./stt/whisper-large-v3-turbo.json";
import tts_qwen3_tts_1_7b_base_q4_k_m from "./tts/qwen3-tts-1.7b-base-q4_k_m.json";
import tts_kokoro_82m_v1_0_q8 from "./tts/kokoro-82m-v1.0-q8.json";
import image_z_image_turbo_q4_0 from "./image/z-image-turbo-q4_0.json";
import image_flux2_klein_4b_q4_0 from "./image/flux2-klein-4b-q4_0.json";
import image_dreamshaper_8_lcm from "./image/dreamshaper-8-lcm.json";
import image_juggernaut_xl_v9 from "./image/juggernaut-xl-v9.json";
import video_fastwan2_2_ti2v_5b_q6_k from "./video/fastwan2.2-ti2v-5b-q6_k.json";
import video_wan2_2_s2v_14b_fp8 from "./video/wan2.2-s2v-14b-fp8.json";

/** Unvalidated catalog entries in catalog order; see `validateCatalog`. */
export const CATALOG_DATA: readonly unknown[] = [
  llm_qwen3_5_4b_q4_k_m,
  llm_qwen3_5_9b_q4_k_m,
  llm_gpt_oss_20b_q4_k_m,
  llm_qwen3_8_27b_q4_k_m,
  llm_qwen3_embedding_0_6b_q8_0,
  llm_qwen3_embedding_4b_q4_k_m,
  llm_clef_flash_q4_k_m,
  llm_clef_q4_k_m,
  stt_whisper_base_q8_0,
  stt_whisper_large_v3_turbo,
  tts_qwen3_tts_1_7b_base_q4_k_m,
  tts_kokoro_82m_v1_0_q8,
  image_z_image_turbo_q4_0,
  image_flux2_klein_4b_q4_0,
  image_dreamshaper_8_lcm,
  image_juggernaut_xl_v9,
  video_fastwan2_2_ti2v_5b_q6_k,
  video_wan2_2_s2v_14b_fp8,
];
