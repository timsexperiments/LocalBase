/**
 * Static index of the catalog data files.
 *
 * Bun has no glob imports, so every model file is imported explicitly here.
 * The array order is the catalog order (it breaks recommendation ties), so
 * append new models where they belong rather than sorting this file.
 * `src/catalog/models/index.test.ts` fails when a data file is missing here.
 */
import llm_qwen3_5_0_8b_q4_k_m from "./llm/qwen3.5-0.8b-q4_k_m.json";
import llm_qwen3_5_0_8b_q8_0 from "./llm/qwen3.5-0.8b-q8_0.json";
import llm_qwen3_5_2b_q4_k_m from "./llm/qwen3.5-2b-q4_k_m.json";
import llm_qwen3_5_2b_q8_0 from "./llm/qwen3.5-2b-q8_0.json";
import llm_qwen3_5_4b_q4_k_m from "./llm/qwen3.5-4b-q4_k_m.json";
import llm_qwen3_5_9b_q4_k_m from "./llm/qwen3.5-9b-q4_k_m.json";
import llm_qwen3_embedding_4b_q4_k_m from "./llm/qwen3-embedding-4b-q4_k_m.json";
import llm_qwen3_embedding_4b_q8_0 from "./llm/qwen3-embedding-4b-q8_0.json";
import llm_qwen3_embedding_8b_q4_k_m from "./llm/qwen3-embedding-8b-q4_k_m.json";
import llm_qwen3_embedding_8b_q8_0 from "./llm/qwen3-embedding-8b-q8_0.json";
import stt_whisper_tiny_q8_0 from "./stt/whisper-tiny-q8_0.json";
import stt_whisper_base_en_q8_0 from "./stt/whisper-base-en-q8_0.json";
import stt_whisper_small_q8_0 from "./stt/whisper-small-q8_0.json";
import stt_whisper_medium_q8_0 from "./stt/whisper-medium-q8_0.json";
import image_realvis_xl_v5_0 from "./image/realvis-xl-v5.0.json";
import llm_qwen3_embedding_0_6b_q8_0 from "./llm/qwen3-embedding-0.6b-q8_0.json";
import llm_qwen2_5_coder_1_5b_instruct_q4_k_m from "./llm/qwen2.5-coder-1.5b-instruct-q4_k_m.json";
import llm_qwen2_5_coder_3b_instruct_q4_k_m from "./llm/qwen2.5-coder-3b-instruct-q4_k_m.json";
import llm_qwen2_5_coder_7b_instruct_q4_k_m from "./llm/qwen2.5-coder-7b-instruct-q4_k_m.json";
import llm_qwen2_5_coder_14b_instruct_q4_k_m from "./llm/qwen2.5-coder-14b-instruct-q4_k_m.json";
import llm_qwen2_5_coder_32b_instruct_q4_k_m from "./llm/qwen2.5-coder-32b-instruct-q4_k_m.json";
import llm_qwen3_coder_next_q4_k_m from "./llm/qwen3-coder-next-q4_k_m.json";
import llm_llama_3_2_1b_instruct_q4_k_m from "./llm/llama-3.2-1b-instruct-q4_k_m.json";
import llm_llama_3_2_3b_instruct_q4_k_m from "./llm/llama-3.2-3b-instruct-q4_k_m.json";
import llm_llama_3_1_8b_instruct_q4_k_m from "./llm/llama-3.1-8b-instruct-q4_k_m.json";
import llm_llama_3_3_70b_instruct_q4_k_m from "./llm/llama-3.3-70b-instruct-q4_k_m.json";
import llm_deepseek_r1_distill_qwen_14b_q4_k_m from "./llm/deepseek-r1-distill-qwen-14b-q4_k_m.json";
import llm_deepseek_coder_6_7b_instruct_q4_k_m from "./llm/deepseek-coder-6.7b-instruct-q4_k_m.json";
import llm_deepseek_coder_v2_lite_instruct_q4_k_m from "./llm/deepseek-coder-v2-lite-instruct-q4_k_m.json";
import llm_deepseek_coder_33b_instruct_q4_k_m from "./llm/deepseek-coder-33b-instruct-q4_k_m.json";
import llm_gemma_3_1b_it_q4_k_m from "./llm/gemma-3-1b-it-q4_k_m.json";
import llm_gemma_3_4b_it_q4_k_m from "./llm/gemma-3-4b-it-q4_k_m.json";
import llm_gemma_3_12b_it_q4_k_m from "./llm/gemma-3-12b-it-q4_k_m.json";
import llm_gemma_3_27b_it_q4_k_m from "./llm/gemma-3-27b-it-q4_k_m.json";
import llm_phi_4_q4_k_m from "./llm/phi-4-q4_k_m.json";
import llm_phi_3_5_mini_instruct_q4_k_m from "./llm/phi-3.5-mini-instruct-q4_k_m.json";
import llm_phi_3_5_moe_instruct_q4_k_m from "./llm/phi-3.5-moe-instruct-q4_k_m.json";
import llm_gpt_oss_20b_q4_k_m from "./llm/gpt-oss-20b-q4_k_m.json";
import llm_falcon_2_11b_instruct_q4_k_m from "./llm/falcon-2-11b-instruct-q4_k_m.json";
import llm_deepseek_r1_distill_qwen_32b_q4_k_m from "./llm/deepseek-r1-distill-qwen-32b-q4_k_m.json";
import llm_deepseek_r1_distill_llama_8b_q4_k_m from "./llm/deepseek-r1-distill-llama-8b-q4_k_m.json";
import llm_deepseek_r1_distill_llama_70b_q4_k_m from "./llm/deepseek-r1-distill-llama-70b-q4_k_m.json";
import llm_qwen2_5_72b_instruct_q4_k_m from "./llm/qwen2.5-72b-instruct-q4_k_m.json";
import llm_codestral_22b_instruct_q4_k_m from "./llm/codestral-22b-instruct-q4_k_m.json";
import llm_mistral_nemo_12b_instruct_q4_k_m from "./llm/mistral-nemo-12b-instruct-q4_k_m.json";
import llm_qwen3_8_27b_q4_k_m from "./llm/qwen3.8-27b-q4_k_m.json";
import llm_devstral_small_2_24b_instruct_q4_k_m from "./llm/devstral-small-2-24b-instruct-q4_k_m.json";
import llm_ministral_3_14b_instruct_q4_k_m from "./llm/ministral-3-14b-instruct-q4_k_m.json";
import llm_qwen3_5_27b_q4_k_m from "./llm/qwen3.5-27b-q4_k_m.json";
import llm_mistral_small_3_2_24b_instruct_q4_k_m from "./llm/mistral-small-3.2-24b-instruct-q4_k_m.json";
import stt_whisper_large_v3_turbo from "./stt/whisper-large-v3-turbo.json";
import stt_whisper_tiny_en_q8_0 from "./stt/whisper-tiny-en-q8_0.json";
import stt_whisper_base_q8_0 from "./stt/whisper-base-q8_0.json";
import tts_qwen3_tts_1_7b_base_q4_k_m from "./tts/qwen3-tts-1.7b-base-q4_k_m.json";
import tts_kokoro_82m_v1_0_q8 from "./tts/kokoro-82m-v1.0-q8.json";
// flux1-ae.safetensors is byte-identical to the ae.safetensors in the gated
// black-forest-labs/FLUX.1-schnell repository (Apache-2.0).
import image_flux1_schnell_q4_0 from "./image/flux1-schnell-q4_0.json";
import image_flux2_klein_4b_q4_0 from "./image/flux2-klein-4b-q4_0.json";
import image_flux2_klein_4b_q8_0 from "./image/flux2-klein-4b-q8_0.json";
import image_flux2_klein_9b_q4_0 from "./image/flux2-klein-9b-q4_0.json";
import image_z_image_turbo_q4_0 from "./image/z-image-turbo-q4_0.json";
import image_stable_diffusion_v1_5 from "./image/stable-diffusion-v1-5.json";
import image_dreamshaper_8_lcm from "./image/dreamshaper-8-lcm.json";
import image_dreamshaper_v8 from "./image/dreamshaper-v8.json";
import image_sdxl_turbo from "./image/sdxl-turbo.json";
import image_sdxl_base_1_0 from "./image/sdxl-base-1.0.json";
import image_juggernaut_xl_v9 from "./image/juggernaut-xl-v9.json";
import image_animagine_xl_3_1 from "./image/animagine-xl-3.1.json";
import image_realvis_xl_v4_0 from "./image/realvis-xl-v4.0.json";
import video_wan2_1_t2v_1_3b_q8_0 from "./video/wan2.1-t2v-1.3b-q8_0.json";
import video_wan2_2_s2v_14b_fp8 from "./video/wan2.2-s2v-14b-fp8.json";
import video_fastwan2_2_ti2v_5b_q6_k from "./video/fastwan2.2-ti2v-5b-q6_k.json";
import video_wan2_2_ti2v_5b_q6_k from "./video/wan2.2-ti2v-5b-q6_k.json";
import video_wan2_2_t2v_a14b_q4_k_m from "./video/wan2.2-t2v-a14b-q4_k_m.json";
import video_ltx_2_3_22b_distilled_1_1_q4_k_m from "./video/ltx-2.3-22b-distilled-1.1-q4_k_m.json";
import video_hunyuanvideo_1_5_t2v_480p_q4_k_m from "./video/hunyuanvideo-1.5-t2v-480p-q4_k_m.json";

/** Unvalidated catalog entries in catalog order; see `validateCatalog`. */
export const CATALOG_DATA: readonly unknown[] = [
  llm_qwen3_5_0_8b_q4_k_m,
  llm_qwen3_5_0_8b_q8_0,
  llm_qwen3_5_2b_q4_k_m,
  llm_qwen3_5_2b_q8_0,
  llm_qwen3_5_4b_q4_k_m,
  llm_qwen3_5_9b_q4_k_m,
  llm_qwen3_embedding_4b_q4_k_m,
  llm_qwen3_embedding_4b_q8_0,
  llm_qwen3_embedding_8b_q4_k_m,
  llm_qwen3_embedding_8b_q8_0,
  stt_whisper_tiny_q8_0,
  stt_whisper_base_en_q8_0,
  stt_whisper_small_q8_0,
  stt_whisper_medium_q8_0,
  image_realvis_xl_v5_0,
  llm_qwen3_embedding_0_6b_q8_0,
  llm_qwen2_5_coder_1_5b_instruct_q4_k_m,
  llm_qwen2_5_coder_3b_instruct_q4_k_m,
  llm_qwen2_5_coder_7b_instruct_q4_k_m,
  llm_qwen2_5_coder_14b_instruct_q4_k_m,
  llm_qwen2_5_coder_32b_instruct_q4_k_m,
  llm_qwen3_coder_next_q4_k_m,
  llm_llama_3_2_1b_instruct_q4_k_m,
  llm_llama_3_2_3b_instruct_q4_k_m,
  llm_llama_3_1_8b_instruct_q4_k_m,
  llm_llama_3_3_70b_instruct_q4_k_m,
  llm_deepseek_r1_distill_qwen_14b_q4_k_m,
  llm_deepseek_coder_6_7b_instruct_q4_k_m,
  llm_deepseek_coder_v2_lite_instruct_q4_k_m,
  llm_deepseek_coder_33b_instruct_q4_k_m,
  llm_gemma_3_1b_it_q4_k_m,
  llm_gemma_3_4b_it_q4_k_m,
  llm_gemma_3_12b_it_q4_k_m,
  llm_gemma_3_27b_it_q4_k_m,
  llm_phi_4_q4_k_m,
  llm_phi_3_5_mini_instruct_q4_k_m,
  llm_phi_3_5_moe_instruct_q4_k_m,
  llm_gpt_oss_20b_q4_k_m,
  llm_falcon_2_11b_instruct_q4_k_m,
  llm_deepseek_r1_distill_qwen_32b_q4_k_m,
  llm_deepseek_r1_distill_llama_8b_q4_k_m,
  llm_deepseek_r1_distill_llama_70b_q4_k_m,
  llm_qwen2_5_72b_instruct_q4_k_m,
  llm_codestral_22b_instruct_q4_k_m,
  llm_mistral_nemo_12b_instruct_q4_k_m,
  llm_qwen3_8_27b_q4_k_m,
  llm_devstral_small_2_24b_instruct_q4_k_m,
  llm_ministral_3_14b_instruct_q4_k_m,
  llm_qwen3_5_27b_q4_k_m,
  llm_mistral_small_3_2_24b_instruct_q4_k_m,
  stt_whisper_large_v3_turbo,
  stt_whisper_tiny_en_q8_0,
  stt_whisper_base_q8_0,
  tts_qwen3_tts_1_7b_base_q4_k_m,
  tts_kokoro_82m_v1_0_q8,
  image_flux1_schnell_q4_0,
  image_flux2_klein_4b_q4_0,
  image_flux2_klein_4b_q8_0,
  image_flux2_klein_9b_q4_0,
  image_z_image_turbo_q4_0,
  image_stable_diffusion_v1_5,
  image_dreamshaper_8_lcm,
  image_dreamshaper_v8,
  image_sdxl_turbo,
  image_sdxl_base_1_0,
  image_juggernaut_xl_v9,
  image_animagine_xl_3_1,
  image_realvis_xl_v4_0,
  video_wan2_1_t2v_1_3b_q8_0,
  video_wan2_2_s2v_14b_fp8,
  video_fastwan2_2_ti2v_5b_q6_k,
  video_wan2_2_ti2v_5b_q6_k,
  video_wan2_2_t2v_a14b_q4_k_m,
  video_ltx_2_3_22b_distilled_1_1_q4_k_m,
  video_hunyuanvideo_1_5_t2v_480p_q4_k_m,
];
