// LocalBase tts-server: a persistent HTTP wrapper around the llama.cpp
// text-to-speech pipeline (tools/tts). The model and projector stay loaded
// between requests, and audio is streamed as raw PCM while it is generated.
//
// API (loopback only, one generation at a time):
//   GET  /health        -> 200 {"status":"ok"} once the model is loaded
//   GET  /slots         -> 200 [{"id":0,"is_processing":bool}]
//   POST /v1/generate   -> JSON {input, voice?, lang?, max_frames?, seed?}
//                          200 chunked audio/L16 (s16le, mono) at the
//                          pipeline sample rate. The terminating chunk is only
//                          sent after a successful generation, so a truncated
//                          body always means failure. The trailer
//                          X-Generated-Frames carries the frame count.
//                          503 when another generation is running.
//
// Audio is flushed at the pipeline's own vocoder window boundaries, so the
// streamed PCM is identical to what the one-shot llama-tts binary writes.

#include "arg.h"
#include "common.h"
#include "log.h"
#include "llama.h"
#include "mtmd.h"
#include "mtmd-helper.h"
#include "sampling.h"

#include <cpp-httplib/httplib.h>
#include <nlohmann/json.hpp>

#include <algorithm>
#include <atomic>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <vector>

using json = nlohmann::ordered_json;

namespace {

// qwen3-tts flushes code2wav every 72 frames. Pulling output at any other
// point would force an early flush and change the waveform.
constexpr int    QWEN3_WINDOW_FRAMES = 72;
// Other pipelines flush on internal boundaries that do not map to frame
// counts, so their audio is emitted once, at the end.

constexpr size_t MAX_INPUT_BYTES     = 16 * 1024;

struct server_args {
    std::string                        host = "127.0.0.1";
    int                                port = 0;
    std::map<std::string, std::string> voices;
};

struct tts_runtime {
    common_params                   params;
    common_init_result_ptr          llama_init;
    llama_model *                   model = nullptr;
    llama_context *                 lctx  = nullptr;
    mtmd::context_ptr               mctx;
    std::map<std::string, mtmd::bitmap_ptr> speakers;
    std::unique_ptr<mtmd_helper::gen_audio> gen;
    int                             sample_rate = 0;
    int                             window_frames = 0;
    std::mutex                      busy;
    std::atomic<bool>               processing{false};
};

void print_usage(int, char ** argv) {
    LOG("\nusage:\n");
    LOG("\n    %s --host 127.0.0.1 --port 8080 -m backbone.gguf -mm mmproj.gguf [--voice name=speaker.wav]...\n", argv[0]);
    LOG("\n");
}

bool extract_server_args(int & argc, char ** argv, server_args & out) {
    int w = 1;
    for (int i = 1; i < argc; i++) {
        const std::string a = argv[i];
        if (a == "--host" || a == "--port" || a == "--voice") {
            if (i + 1 >= argc) {
                fprintf(stderr, "error: %s requires a value\n", a.c_str());
                return false;
            }
            const std::string v = argv[++i];
            if (a == "--host") {
                out.host = v;
            } else if (a == "--port") {
                try {
                    out.port = std::stoi(v);
                } catch (...) {
                    fprintf(stderr, "error: invalid --port %s\n", v.c_str());
                    return false;
                }
                if (out.port <= 0 || out.port > 65535) {
                    fprintf(stderr, "error: invalid --port %s\n", v.c_str());
                    return false;
                }
            } else {
                const auto eq = v.find('=');
                if (eq == std::string::npos || eq == 0 || eq + 1 >= v.size()) {
                    fprintf(stderr, "error: --voice expects name=path, got %s\n", v.c_str());
                    return false;
                }
                out.voices[v.substr(0, eq)] = v.substr(eq + 1);
            }
            continue;
        }
        argv[w++] = argv[i];
    }
    argc = w;
    argv[argc] = nullptr;
    return true;
}

void send_error(httplib::Response & res, int status, const std::string & message) {
    res.status = status;
    res.set_content(json{{"error", message}}.dump(), "application/json");
}

void append_s16(std::string & out, const float * pcm, size_t n) {
    const size_t base = out.size();
    out.resize(base + n * 2);
    for (size_t i = 0; i < n; i++) {
        const float   v = std::max(-1.0f, std::min(1.0f, pcm[i]));
        const int16_t s = (int16_t) (v * 32767.0f);
        out[base + 2 * i]     = (char) (s & 0xff);
        out[base + 2 * i + 1] = (char) ((s >> 8) & 0xff);
    }
}

// One generation. Lives until the chunked provider finishes; holds the busy
// lock for its whole lifetime.
struct generation {
    tts_runtime &                rt;
    std::unique_lock<std::mutex> lock;
    common_sampler *             smpl = nullptr;
    std::string                  prompt;
    std::string                  lang;
    int                          max_frames = 0;
    uint32_t                     seed       = LLAMA_DEFAULT_SEED;
    mtmd_bitmap *                speaker    = nullptr;

    llama_token   sampled  = LLAMA_TOKEN_NULL;
    const float * h_state  = nullptr;
    int           n_frames = 0;
    size_t        emitted  = 0;
    bool          finished = false;
    int64_t       t_start_us = 0;
    int64_t       t_first_audio_us = 0;

    generation(tts_runtime & r, std::unique_lock<std::mutex> && l) : rt(r), lock(std::move(l)) {
        rt.processing = true;
    }
    ~generation() {
        release();
        if (smpl) {
            common_sampler_free(smpl);
        }
    }

    void release() {
        if (lock.owns_lock()) {
            rt.processing = false;
            lock.unlock();
        }
    }

    llama_token sample() {
        llama_token t = common_sampler_sample(smpl, rt.lctx, -1);
        common_sampler_accept(smpl, t, true);
        return t;
    }

    // set_input + prompt processing + first sample. Returns an error message
    // or empty on success.
    std::string start() {
        t_start_us = ggml_time_us();
        llama_memory_clear(llama_get_memory(rt.lctx), true);

        common_params_sampling sparams = rt.params.sampling;
        sparams.seed = seed;
        smpl = common_sampler_init(rt.model, sparams);
        if (!smpl) {
            return "failed to initialize sampler";
        }

        mtmd_helper_gen_audio_inp inp{};
        inp.seq_id      = 0;
        inp.prompt      = prompt.c_str();
        inp.prompt_len  = prompt.size();
        inp.speaker_ref = speaker;
        inp.lang        = lang.c_str();
        inp.top_k       = sparams.top_k;
        inp.top_p       = sparams.top_p;
        inp.seed        = seed;
        inp.out_type    = MTMD_HELPER_GEN_AUDIO_OUTTYPE_PCM;

        if (rt.gen->set_input(&inp) != 0) {
            return "set_input failed";
        }
        for (;;) {
            const int32_t ret = rt.gen->step_prompt(rt.params.n_batch);
            if (ret < 0) {
                return "prompt processing failed";
            }
            if (ret == 0) {
                break;
            }
        }
        sampled = sample();
        h_state = llama_get_embeddings_ith(rt.lctx, -1);
        if (!h_state) {
            return "missing hidden state after prompt";
        }
        return "";
    }

    // Collects PCM produced since the last call into out (s16le).
    bool drain(std::string & out) {
        int32_t      rate = 0;
        const char * data = nullptr;
        size_t       len  = 0;
        int64_t      n    = 0;
        if (rt.gen->get_output(&rate, &data, &len, &n) != 0) {
            LOG_ERR("get_output failed\n");
            return false;
        }
        const auto * pcm = (const float *) data;
        const size_t total = len / sizeof(float);
        if (total > emitted) {
            append_s16(out, pcm + emitted, total - emitted);
            emitted = total;
        }
        return true;
    }

    // Advances generation until a chunk of audio is available or generation
    // ends. Returns false on failure.
    bool next_chunk(std::string & out) {
        bool stop = false;
        while (!stop && n_frames < max_frames) {
            const float * h_next = nullptr;
            if (rt.gen->step_gen(sampled, h_state, &h_next, &stop) != 0) {
                LOG_ERR("step_gen failed at frame %d\n", n_frames);
                return false;
            }
            if (!h_next) {
                break;
            }
            n_frames++;
            h_state = h_next;
            sampled = sample();
            // the pipeline decodes a code2wav window every window_frames frames; draining
            // at any other point would force a partial window and change the audio
            if (rt.window_frames > 0 && n_frames % rt.window_frames == 0) {
                if (!drain(out)) {
                    return false;
                }
                if (!out.empty()) {
                    return true;
                }
            }
        }
        finished = true;
        return drain(out);
    }
};

} // namespace

int main(int argc, char ** argv) {
    server_args sargs;
    if (!extract_server_args(argc, argv, sargs)) {
        return 1;
    }
    if (sargs.port == 0) {
        fprintf(stderr, "error: --port is required\n");
        return 1;
    }

    auto rt = std::make_unique<tts_runtime>();
    common_params & params = rt->params;

    common_init();
    if (!common_params_parse(argc, argv, params, LLAMA_EXAMPLE_TTS, print_usage)) {
        return 1;
    }
    mtmd_helper_log_set(common_log_default_callback, nullptr);

    if (params.mmproj.path.empty()) {
        LOG_ERR("no mmproj provided, use --mmproj\n");
        return 1;
    }
    params.embedding = true;

    llama_backend_init();
    llama_numa_init(params.numa);

    rt->llama_init = common_init_from_params(params);
    rt->model      = rt->llama_init->model();
    rt->lctx       = rt->llama_init->context();
    if (!rt->model || !rt->lctx) {
        LOG_ERR("failed to init model/context\n");
        return 1;
    }

    mtmd_context_params mparams = mtmd_context_params_default();
    mparams.use_gpu = params.mmproj_use_gpu;
    rt->mctx.reset(mtmd_init_from_file(params.mmproj.path.c_str(), rt->model, mparams));
    if (!rt->mctx) {
        LOG_ERR("failed to load mmproj %s\n", params.mmproj.path.c_str());
        return 1;
    }
    const mtmd_gen_audio_info info = mtmd_gen_audio_get_info(rt->mctx.get());
    if (info.type == MTMD_GEN_AUDIO_TYPE_NONE) {
        LOG_ERR("mmproj does not support audio generation\n");
        return 1;
    }
    rt->sample_rate   = info.sample_rate;
    rt->window_frames = info.type == MTMD_GEN_AUDIO_TYPE_QWEN3TTS ? QWEN3_WINDOW_FRAMES : 0;

    for (const auto & [name, path] : sargs.voices) {
        auto wrapper = mtmd_helper_bitmap_init_from_file(rt->mctx.get(), path.c_str(), false);
        if (!wrapper.bitmap) {
            LOG_ERR("failed to load speaker file for voice %s\n", name.c_str());
            return 1;
        }
        rt->speakers[name].reset(wrapper.bitmap);
    }

    rt->gen = std::make_unique<mtmd_helper::gen_audio>(rt->lctx, rt->mctx.get());

    const int default_frames = params.n_predict > 0 ? params.n_predict : 512;

    httplib::Server svr;
    svr.set_payload_max_length(64 * 1024);
    svr.new_task_queue = [] { return new httplib::ThreadPool(4); };

    svr.Get("/health", [&](const httplib::Request &, httplib::Response & res) {
        res.set_content(json{{"status", "ok"}, {"sample_rate", rt->sample_rate}}.dump(), "application/json");
    });

    // llama-server compatible slot view, so the gateway can tell whether a
    // cancelled request is still being generated.
    svr.Get("/slots", [&](const httplib::Request &, httplib::Response & res) {
        res.set_content(json::array({json{{"id", 0}, {"is_processing", rt->processing.load()}}}).dump(), "application/json");
    });

    svr.Post("/v1/generate", [&](const httplib::Request & req, httplib::Response & res) {
        json body;
        try {
            body = json::parse(req.body);
        } catch (const std::exception &) {
            send_error(res, 400, "request body must be JSON");
            return;
        }
        if (!body.is_object() || !body.contains("input") || !body["input"].is_string()) {
            send_error(res, 400, "input must be a string");
            return;
        }
        const std::string input = body["input"].get<std::string>();
        if (input.empty() || input.size() > MAX_INPUT_BYTES) {
            send_error(res, 400, "input must be non-empty and at most 16 KiB");
            return;
        }

        mtmd_bitmap * speaker = nullptr;
        if (body.contains("voice") && !body["voice"].is_null()) {
            if (!body["voice"].is_string()) {
                send_error(res, 400, "voice must be a string");
                return;
            }
            const std::string voice = body["voice"].get<std::string>();
            if (!voice.empty() && voice != "default") {
                auto it = rt->speakers.find(voice);
                if (it == rt->speakers.end()) {
                    send_error(res, 400, "unknown voice");
                    return;
                }
                speaker = it->second.get();
            }
        }

        std::string lang = params.tts_lang;
        if (body.contains("lang") && body["lang"].is_string()) {
            lang = body["lang"].get<std::string>();
        }

        int max_frames = default_frames;
        if (body.contains("max_frames") && !body["max_frames"].is_null()) {
            if (!body["max_frames"].is_number_integer()) {
                send_error(res, 400, "max_frames must be an integer");
                return;
            }
            const int64_t requested = body["max_frames"].get<int64_t>();
            if (requested <= 0) {
                send_error(res, 400, "max_frames must be positive");
                return;
            }
            max_frames = (int) std::min<int64_t>(requested, default_frames);
        }

        uint32_t seed = params.sampling.seed;
        if (body.contains("seed") && !body["seed"].is_null()) {
            if (!body["seed"].is_number_unsigned()) {
                send_error(res, 400, "seed must be a non-negative integer");
                return;
            }
            seed = (uint32_t) body["seed"].get<uint64_t>();
        }

        std::unique_lock<std::mutex> lock(rt->busy, std::try_to_lock);
        if (!lock.owns_lock()) {
            send_error(res, 503, "busy");
            return;
        }

        auto g = std::make_shared<generation>(*rt, std::move(lock));
        g->prompt     = input;
        g->lang       = lang;
        g->max_frames = max_frames;
        g->seed       = seed;
        g->speaker    = speaker;

        const std::string err = g->start();
        if (!err.empty()) {
            LOG_ERR("generation failed: %s\n", err.c_str());
            send_error(res, 500, err);
            return;
        }

        res.set_header("X-Sample-Rate", std::to_string(rt->sample_rate));
        res.set_header("X-Audio-Format", "s16le");
        res.set_header("Trailer", "X-Generated-Frames");
        res.set_chunked_content_provider(
            "audio/L16",
            [g](size_t, httplib::DataSink & sink) -> bool {
                if (g->finished) {
                    return false;
                }
                std::string chunk;
                if (!g->next_chunk(chunk)) {
                    return false;
                }
                if (!chunk.empty()) {
                    if (g->t_first_audio_us == 0) {
                        g->t_first_audio_us = ggml_time_us();
                    }
                    if (!sink.is_writable() || !sink.write(chunk.data(), chunk.size())) {
                        LOG_WRN("client disconnected, stopping generation at frame %d\n", g->n_frames);
                        return false;
                    }
                }
                if (g->finished) {
                    const double total_s = (ggml_time_us() - g->t_start_us) / 1e6;
                    const double first_s = g->t_first_audio_us ? (g->t_first_audio_us - g->t_start_us) / 1e6 : 0.0;
                    LOG_INF("generated %d frames, %zu samples (%d Hz), first audio %.2fs, total %.2fs\n",
                            g->n_frames, g->emitted, g->rt.sample_rate, first_s, total_s);
                    httplib::Headers trailer = {{"X-Generated-Frames", std::to_string(g->n_frames)}};
                    sink.done_with_trailer(trailer);
                }
                return true;
            },
            [g](bool) {
                // Release the busy lock as soon as the response is complete.
                g->release();
            });
    });

    if (!svr.bind_to_port(sargs.host, sargs.port)) {
        LOG_ERR("failed to bind %s:%d\n", sargs.host.c_str(), sargs.port);
        return 1;
    }
    LOG_INF("tts-server listening on %s:%d\n", sargs.host.c_str(), sargs.port);
    fflush(stdout);
    fflush(stderr);
    svr.listen_after_bind();

    llama_backend_free();
    return 0;
}
