#!/usr/bin/env python3
"""audio_transcriber 的 Python 子进程（嵌在 Go 二进制里，运行时写到 ~/.local/share/asr/）。

  asr_worker.py asr <16k单声道wav> <输出前缀>   Qwen3-ASR 转写，写 <前缀>.txt 和 <前缀>.srt
  asr_worker.py spk <16k单声道wav> <输出前缀>   pyannote 分说话人，读 <前缀>.srt，写 <前缀>.speakers.txt

环境变量（audio_transcriber 会传好）：
  QWEN_ASR_MODEL / QWEN_ALIGNER_MODEL   两个 Qwen 模型的本地目录
  SPK_MODEL                             pyannote community-1 的本地目录（没有就按仓库名找 HF 缓存）
  QWEN_ASR_BATCH                        一次喂几段（默认 1：实测比 2 段一起还快、显存峰值 6.6G；设大了显存不够会自动降回 1）
  QWEN_ASR_LANG                         强制语种（Chinese / Japanese / English…），不设就自动认
"""
import gc
import os
import re
import sys
import warnings

warnings.filterwarnings("ignore")

SR = 16000
HOME = os.path.expanduser("~")
ASR_DIR = os.environ.get("QWEN_ASR_MODEL") or f"{HOME}/.local/share/asr/models/Qwen3-ASR-1.7B"
ALIGNER_DIR = os.environ.get("QWEN_ALIGNER_MODEL") or f"{HOME}/.local/share/asr/models/Qwen3-ForcedAligner-0.6B"
SPK_DIR = os.environ.get("SPK_MODEL") or f"{HOME}/.local/share/asr/models/speaker-diarization-community-1"
SPK_REPO = "pyannote/speaker-diarization-community-1"

# 对齐模型只认这 11 种
ALIGN_LANGS = {"Chinese", "English", "Cantonese", "French", "German", "Italian",
               "Japanese", "Korean", "Portuguese", "Russian", "Spanish"}
SENT_END = set("。！？!?…")
SOFT_BREAK = set("，、；：,;:")
ABBR = {"dr", "mr", "mrs", "ms", "prof", "st", "vs", "etc", "e.g", "i.e", "no", "jr", "sr"}
CJK = re.compile(r"[぀-ヿ㐀-鿿豈-﫿]")

OOM_HINT = ("显存不够。多半是本地朗读（hub-tts）占着：等它闲 10 分钟自己放掉，"
            "或者 systemctl --user restart hub-tts 立刻放，然后重跑。")


def log(msg):
    print(msg, file=sys.stderr, flush=True)


def load_wav(path):
    import soundfile as sf
    wav, sr = sf.read(path, dtype="float32", always_2d=False)
    if wav.ndim > 1:
        wav = wav.mean(axis=1)
    if sr != SR:
        sys.exit(f"采样率应该是 16000，实际是 {sr}")
    return wav


def is_cjk(text):
    chars = [c for c in text if not c.isspace()]
    if not chars:
        return False
    return sum(1 for c in chars if CJK.match(c)) / len(chars) > 0.3


def ts(t):
    ms = max(0, int(round(t * 1000)))
    return f"{ms // 3600000:02d}:{ms % 3600000 // 60000:02d}:{ms % 60000 // 1000:02d},{ms % 1000:03d}"


def hms(t):
    s = max(0, int(t))
    return f"{s // 3600:02d}:{s % 3600 // 60:02d}:{s % 60:02d}"


# ---------------------------------------------------------------- 转写

def to_units(items, text, offset):
    """把对齐结果（逐字/逐词、不带标点）挂回带标点的原文：每个单位 = [字词, 起, 止, 后面跟的标点空格]。"""
    units = []
    low = text.lower()
    pos = 0
    for it in items:
        t = (it.text or "").strip()
        if not t:
            continue
        idx = low.find(t.lower(), pos)
        if idx == -1:
            units.append([t, it.start_time + offset, it.end_time + offset, ""])
            continue
        if units:
            units[-1][3] += text[pos:idx]
        units.append([text[idx:idx + len(t)], it.start_time + offset, it.end_time + offset, ""])
        pos = idx + len(t)
    if units:
        units[-1][3] += text[pos:]
    return units


def to_cues(units, cjk):
    """按句末标点切；太长就在逗号处切；实在没标点才按长度/时长硬切（中文硬切会切在词中间）。"""
    max_len = 26 if cjk else 60
    cues, cur = [], None
    for word, st, en, trail in units:
        if cur is not None and st - cur["end"] > 1.0:
            cues.append(cur)
            cur = None
        if cur is None:
            cur = {"start": st, "end": en, "text": ""}
        cur["text"] += word + trail
        cur["end"] = max(en, cur["end"])
        n = len(cur["text"].strip())
        sentence_end = any(c in SENT_END for c in trail) or (
            not cjk and "." in trail and word.lower().rstrip(".") not in ABBR)
        soft = any(c in SOFT_BREAK for c in trail) and n >= max_len * 0.5
        hard = n >= max_len * 1.6 or cur["end"] - cur["start"] >= 10.0
        if sentence_end or soft or hard:
            cues.append(cur)
            cur = None
    if cur is not None:
        cues.append(cur)
    return [c for c in cues if c["text"].strip()]


def run_asr(wav_path, outbase):
    import torch
    from qwen_asr import Qwen3ASRModel, Qwen3ForcedAligner
    from qwen_asr.inference.utils import MAX_FORCE_ALIGN_INPUT_SECONDS, split_audio_into_chunks

    wav = load_wav(wav_path)
    # 对齐模型一次最多 3 分钟，所以先按 3 分钟、找安静处切开；识别和对齐都按这些段来
    chunks = split_audio_into_chunks(wav=wav, sr=SR, max_chunk_sec=MAX_FORCE_ALIGN_INPUT_SECONDS)
    lang = os.environ.get("QWEN_ASR_LANG") or None
    batch = max(1, int(os.environ.get("QWEN_ASR_BATCH") or 1))
    log(f"音频 {len(wav) / SR / 60:.1f} 分钟，切成 {len(chunks)} 段；加载 Qwen3-ASR…")

    try:
        asr = Qwen3ASRModel.from_pretrained(ASR_DIR, dtype=torch.bfloat16, device_map="cuda:0",
                                            max_inference_batch_size=batch, max_new_tokens=2048)
        texts, langs = [], []
        i = 0
        while i < len(chunks):
            part = chunks[i:i + batch]
            try:
                res = asr.transcribe(audio=[(c, SR) for c, _ in part], language=lang)
            except torch.OutOfMemoryError:
                if batch == 1:
                    raise
                torch.cuda.empty_cache()
                batch = 1
                asr.max_inference_batch_size = 1
                log("显存紧，改成一次一段")
                continue
            texts += [r.text for r in res]
            langs += [r.language for r in res]
            i += len(part)
            log(f"识别 {i}/{len(chunks)}")
        del asr
        gc.collect()
        torch.cuda.empty_cache()

        log("加载对齐模型，给每个字对时间…")
        aligner = Qwen3ForcedAligner.from_pretrained(ALIGNER_DIR, dtype=torch.bfloat16, device_map="cuda:0")
        cues = []
        for k, ((cwav, off), text, lng) in enumerate(zip(chunks, texts, langs)):
            text = (text or "").strip()
            if not text:
                continue
            cands = [x.strip() for x in str(lng).split(",") if x.strip() in ALIGN_LANGS]
            units = None
            if cands:
                try:
                    al = aligner.align(audio=[(cwav, SR)], text=[text], language=[cands[0]])[0]
                    units = to_units(al.items, text, off)
                except torch.OutOfMemoryError:
                    raise
                except Exception as e:  # 对不上就退回整段一条，别让一段毁掉整集
                    log(f"第 {k + 1} 段对时间轴失败（{e}），这段按一整条写")
            if not units:
                units = [[text, off, off + len(cwav) / SR, ""]]
            cues += to_cues(units, is_cjk(text))
    except torch.OutOfMemoryError:
        log(OOM_HINT)
        sys.exit(3)

    with open(outbase + ".srt", "w", encoding="utf-8") as f:
        for n, c in enumerate(cues, 1):
            end = max(c["end"], c["start"] + 0.3)
            f.write(f"{n}\n{ts(c['start'])} --> {ts(end)}\n{c['text'].strip()}\n\n")
    with open(outbase + ".txt", "w", encoding="utf-8") as f:
        for c in cues:
            f.write(c["text"].strip() + "\n")
    log(f"写好 {len(cues)} 条字幕；认出的语种：{','.join(sorted(set(langs))) or '无'}")


# ---------------------------------------------------------------- 分说话人

SRT_BLOCK = re.compile(r"(\d+):(\d+):(\d+)[,.](\d+)\s*-->\s*(\d+):(\d+):(\d+)[,.](\d+)[^\n]*\n(.*?)(?:\n\s*\n|\Z)", re.S)


def read_srt(path):
    with open(path, encoding="utf-8") as f:
        data = f.read().replace("\r\n", "\n")
    cues = []
    for m in SRT_BLOCK.finditer(data):
        g = [int(x) for x in m.groups()[:8]]
        st = g[0] * 3600 + g[1] * 60 + g[2] + g[3] / 1000
        en = g[4] * 3600 + g[5] * 60 + g[6] + g[7] / 1000
        text = " ".join(m.group(9).split("\n")).strip()
        if text:
            cues.append((st, en, text))
    return cues


def run_spk(wav_path, outbase):
    import torch
    from pyannote.audio import Pipeline

    srt = outbase + ".srt"
    if not os.path.isfile(srt):
        sys.exit(f"找不到 {srt}，分说话人要先有转写好的字幕")
    cues = read_srt(srt)
    if not cues:
        sys.exit(f"{srt} 里没有字幕，没法分说话人")

    src = SPK_DIR if os.path.isfile(os.path.join(SPK_DIR, "config.yaml")) else SPK_REPO
    try:
        pipe = Pipeline.from_pretrained(src)
    except Exception as e:
        log(f"加载 pyannote 模型失败：{e}")
        log("要先在 Hugging Face 网页上对 pyannote/speaker-diarization-community-1 点同意，"
            "再在终端跑 hf auth login，然后 hf-get pyannote/speaker-diarization-community-1 "
            "~/.local/share/asr/models/speaker-diarization-community-1")
        sys.exit(4)
    if pipe is None:
        sys.exit("加载 pyannote 模型失败（没登录 Hugging Face 或没点同意）")
    pipe.to(torch.device("cuda"))

    wav = load_wav(wav_path)
    log(f"音频 {len(wav) / SR / 60:.1f} 分钟，分说话人…")
    try:
        out = pipe({"waveform": torch.from_numpy(wav).unsqueeze(0), "sample_rate": SR})
    except torch.OutOfMemoryError:
        log(OOM_HINT)
        sys.exit(3)
    # exclusive：同一时刻只算一个人，抢话的地方判给一个，字幕才能一句贴一个标签
    ann = getattr(out, "exclusive_speaker_diarization", None) or getattr(out, "speaker_diarization", out)
    turns = sorted((seg.start, seg.end, spk) for seg, _, spk in ann.itertracks(yield_label=True))
    if not turns:
        sys.exit("没听出有人说话")

    tagged = []
    for st, en, text in cues:
        score = {}
        for t0, t1, spk in turns:
            if t0 > en:
                break
            o = min(en, t1) - max(st, t0)
            if o > 0:
                score[spk] = score.get(spk, 0) + o
        if score:
            spk = max(score, key=score.get)
        else:  # 字幕落在没人说话的空当里，就跟离它最近的那段
            mid = (st + en) / 2
            spk = min(turns, key=lambda t: min(abs(t[0] - mid), abs(t[1] - mid)))[2]
        tagged.append((st, spk, text))

    names = {}
    for _, spk, _ in tagged:
        if spk not in names:
            k = len(names)
            names[spk] = chr(ord("A") + k) if k < 26 else f"S{k + 1}"

    paras = []
    for st, spk, text in tagged:
        if paras and paras[-1][1] == spk:
            paras[-1][2].append(text)
        else:
            paras.append([st, spk, [text]])

    with open(outbase + ".speakers.txt", "w", encoding="utf-8") as f:
        for st, spk, texts in paras:
            joined = "".join(texts) if is_cjk("".join(texts)) else " ".join(texts)
            f.write(f"[{hms(st)}] {names[spk]}：{joined}\n\n")
    log(f"听出 {len(names)} 个说话人，{len(paras)} 段")


if __name__ == "__main__":
    if len(sys.argv) != 4 or sys.argv[1] not in ("asr", "spk"):
        sys.exit(__doc__)
    {"asr": run_asr, "spk": run_spk}[sys.argv[1]](sys.argv[2], sys.argv[3])
