// Native inference for original faster-whisper CTranslate2 directories.
// Stdout carries UTF-8 JSON lines; logs and model paths never enter subtitles.
#include "features.h"
#include <ctranslate2/models/whisper.h>
#include <iostream>
#include <nlohmann/json.hpp>

using namespace ctranslate2;
using IDs = std::vector<size_t>;
using json = nlohmann::json;
struct Segment {
  int start, end;
  IDs tokens;
};

static std::vector<Segment> split(const IDs &ids, size_t timestamp, size_t eot,
                                  int count, int &advance) {
  std::vector<Segment> segments;
  const bool closed = ids.size() >= 2 && ids.back() >= timestamp &&
                      ids[ids.size() - 2] < timestamp;
  size_t first = 0;
  auto emit = [&](size_t limit, bool timestamp_start) {
    Segment s{0, count, {}};
    if (timestamp_start && ids[first] >= timestamp)
      s.start = std::min(count, int(ids[first] - timestamp) * 2);
    for (size_t j = first; j < limit; ++j) {
      if (ids[j] < eot)
        s.tokens.push_back(ids[j]);
      if (ids[j] >= timestamp && ids[j] > timestamp)
        s.end = std::min(count, int(ids[j] - timestamp) * 2);
    }
    if (!s.tokens.empty())
      segments.push_back(std::move(s));
    first = limit;
  };
  for (size_t i = 1; i < ids.size(); ++i)
    if (ids[i] >= timestamp && ids[i - 1] >= timestamp)
      emit(i, true);
  advance = count;
  if (first == 0 || closed)
    emit(ids.size(), first != 0);
  else if (ids[first - 1] > timestamp)
    advance = std::min(count, int(ids[first - 1] - timestamp) * 2);
  return segments;
}

static void run(const char *model_dir, const char *pcm, std::string lang) {
  models::ModelLoader loader(model_dir);
  loader.compute_type = ComputeType::INT8;
  auto model = std::dynamic_pointer_cast<const models::WhisperModel>(
      loader.load().at(0));
  if (!model)
    throw std::runtime_error("not a Whisper model");
  models::Whisper engine(model);
  const auto &vocab = model->get_vocabulary();
  const auto eot = vocab.eos_id(),
             timestamp = vocab.to_id("<|notimestamps|>", false) + 1;
  WhisperFeatures features(pcm, engine.n_mels());
  features.measure_peak();
  models::WhisperOptions opts;
  opts.beam_size = 5;
  opts.sampling_topk = 1;
  opts.return_scores = true;
  opts.return_no_speech_prob = true;
  IDs history;
  if (!engine.is_multilingual())
    lang = "en";
  for (int64_t seek = 0; seek < features.frames();) {
    int count = int(
        std::min<int64_t>(WhisperFeatures::chunk, features.frames() - seek));
    auto mel = features.window_at(seek, count);
    StorageView input({1, dim_t(engine.n_mels()), WhisperFeatures::chunk}, mel);
    auto encoded = engine.encode(input, false).get();
    if (lang == "auto") {
      auto prediction = engine.detect_language(encoded).at(0).get().at(0).first;
      if (prediction.size() < 5)
        throw std::runtime_error("unknown language");
      lang = prediction.substr(2, prediction.size() - 4);
    }
    if (seek == 0)
      std::cout << json{{"language", lang}}.dump() << '\n';
    IDs start{vocab.bos_id()};
    if (engine.is_multilingual()) {
      start.push_back(vocab.to_id("<|" + lang + "|>", false));
      start.push_back(vocab.to_id("<|transcribe|>", false));
    }
    IDs prompt;
    if (!history.empty()) {
      prompt.push_back(vocab.to_id("<|startofprev|>", false));
      prompt.insert(prompt.end(), history.begin(), history.end());
    }
    prompt.insert(prompt.end(), start.begin(), start.end());
    auto result =
        engine.generate(encoded, std::vector<IDs>{prompt}, opts).at(0).get();
    const auto &ids = result.sequences_ids.at(0);
    const auto avg_score = result.scores.at(0) * ids.size() / (ids.size() + 1);
    if (ids.empty() || (result.no_speech_prob > .6 && avg_score <= -1)) {
      seek += count;
      continue;
    }
    int advance;
    auto segments = split(ids, timestamp, eot, count, advance);
    IDs text;
    for (const auto &seg : segments)
      text.insert(text.end(), seg.tokens.begin(), seg.tokens.end());
    if (!text.empty()) {
      auto alignment =
          engine.align(encoded, start, {text}, {size_t(count)}, 7).at(0).get();
      std::vector<int> boundaries;
      int64_t previous = -1;
      for (auto [token, t] : alignment.alignments) {
        if (token == previous)
          continue;
        if (token != int64_t(boundaries.size()))
          throw std::runtime_error("word alignment order");
        boundaries.push_back(std::clamp(int(t) * 2, 0, count));
        previous = token;
      }
      if (boundaries.size() != text.size() + 1)
        throw std::runtime_error("word alignment length");
      size_t offset = 0;
      for (const auto &seg : segments) {
        auto pieces = vocab.to_tokens({seg.tokens}).at(0);
        std::vector<int64_t> times;
        for (size_t j = 0; j <= pieces.size(); ++j)
          times.push_back((seek + boundaries.at(offset + j)) * 10);
        std::cout << json{{"pieces", pieces},
                          {"times", times},
                          {"start", (seek + seg.start) * 10},
                          {"end", (seek + seg.end) * 10}}
                         .dump()
                  << '\n';
        offset += pieces.size();
      }
      if (advance < count && boundaries.back() > 0)
        advance = boundaries.back();
    }
    // Only completed slices become context. The unfinished tail is
    // decoded again from its timestamp in the next window.
    auto limit = ids.end();
    if (advance < count) {
      for (size_t j = ids.size(); j > 1; --j)
        if (ids[j - 1] >= timestamp && ids[j - 2] >= timestamp) {
          limit = ids.begin() + j - 1;
          break;
        }
    }
    history.insert(history.end(), ids.begin(), limit);
    if (history.size() > 223)
      history.erase(history.begin(), history.end() - 223);
    seek += std::max(1, advance);
    if (!std::cout)
      throw std::runtime_error("output");
  }
}
int main(int argc, char **argv) {
  if (argc != 4)
    return 2;
  try {
    run(argv[1], argv[2], argv[3]);
    return 0;
  } catch (const std::exception &e) {
    std::cerr << "CTranslate2: " << e.what() << '\n';
    return 1;
  }
}
