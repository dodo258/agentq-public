package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 发图片、录屏和文件给主人：图片走 sendPhoto（10MB 内），录屏走 sendVideo，其余走 sendDocument（都是 50MB 内）。
// 录屏超过 50MB 先压缩（有 ffmpeg 用 ffmpeg，没有用系统自带的 avconvert），压不下就改发一句带文件路径的话。
// 用在两处：对话任务里 `agentq notify file`（马上发，回给主人）和 `agentq notify send --file=…`（跟着那条推送发）。

const compressTimeout = 10 * time.Minute

// mediaKind 按扩展名分：photo、video 或 document。
func mediaKind(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return "photo"
	case ".mp4", ".mov", ".m4v":
		return "video"
	}
	return "document"
}

// prepareMedia 检查要发的文件：能直接发返回原路径；录屏超过 50MB 返回压缩后的文件（存到 ~/.agentq/notify/media/）；
// 实在发不了返回 note（一句带文件路径的说明，调用方改发这句话）。文件不在、是目录时返回错误。
func (a *App) prepareMedia(path string) (send, note string, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("文件不在：%s", path)
	}
	if fi.IsDir() {
		return "", "", fmt.Errorf("%s 是目录，不是文件", path)
	}
	if fi.Size() <= tgUploadMax {
		return path, "", nil
	}
	shown := a.tilde(path)
	if mediaKind(path) != "video" {
		return "", fmt.Sprintf("文件 %s 有 %s，超过 Telegram 给机器人的 50 MB 上限，没发；文件在 %s", filepath.Base(path), humanSize(fi.Size()), shown), nil
	}
	compress := a.Compress
	if compress == nil {
		compress = compressVideo
	}
	dir := filepath.Join(a.dataDir(), "notify", "media")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	out, cerr := compress(path, dir)
	if cerr == nil {
		if ofi, err := os.Stat(out); err == nil && ofi.Size() <= tgUploadMax {
			a.logf("推送：录屏 %s 有 %s，压缩到 %s 再发（%s）", shown, humanSize(fi.Size()), humanSize(ofi.Size()), a.tilde(out))
			return out, "", nil
		} else if err == nil {
			cerr = fmt.Errorf("压到 %s 还是超过 50 MB", humanSize(ofi.Size()))
			os.Remove(out) // 发不了，不留着占地方
		} else {
			cerr = err
		}
	}
	return "", fmt.Sprintf("录屏 %s 有 %s，超过 Telegram 给机器人的 50 MB 上限，%s，没发；文件在 %s",
		filepath.Base(path), humanSize(fi.Size()), oneLine(cerr.Error(), 120), shown), nil
}

// compressVideo 把录屏压小一些，返回压得最小的那个结果：有 ffmpeg 用 H.264 先 crf 28 再 crf 32（宽最多 1280），
// 没有就用 /usr/bin/avconvert 依次试 1280x720、960x540、640x480，每次压完不超过 50MB 就停。
func compressVideo(src, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compressTimeout)
	defer cancel()
	base := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)))
	type try struct {
		out  string
		name string
		args []string
	}
	var tries []try
	if ff, err := exec.LookPath("ffmpeg"); err == nil {
		for _, crf := range []string{"28", "32"} {
			out := base + "-crf" + crf + ".mp4"
			tries = append(tries, try{out, ff, []string{"-y", "-loglevel", "error", "-i", src, "-vf", "scale='min(1280,iw)':-2",
				"-c:v", "libx264", "-crf", crf, "-preset", "veryfast", "-c:a", "aac", "-b:a", "96k", "-movflags", "+faststart", out}})
		}
	} else if _, err := os.Stat("/usr/bin/avconvert"); err == nil {
		for _, p := range []string{"Preset1280x720", "Preset960x540", "Preset640x480"} {
			out := base + "-" + strings.TrimPrefix(p, "Preset") + ".mp4"
			tries = append(tries, try{out, "/usr/bin/avconvert", []string{"--source", src, "--preset", p, "--output", out, "--replace"}})
		}
	} else {
		return "", errors.New("这台电脑上没有 ffmpeg 也没有 avconvert，压不了")
	}
	best, bestSize := "", int64(-1)
	var lastErr error
	for _, t := range tries {
		if out, err := exec.CommandContext(ctx, t.name, t.args...).CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("%s 压缩失败：%v %s", filepath.Base(t.name), err, oneLine(string(out), 120))
			os.Remove(t.out)
			continue
		}
		fi, err := os.Stat(t.out)
		if err != nil {
			lastErr = err
			continue
		}
		if bestSize < 0 || fi.Size() < bestSize {
			if best != "" {
				os.Remove(best)
			}
			best, bestSize = t.out, fi.Size()
		} else {
			os.Remove(t.out)
		}
		if bestSize <= tgUploadMax {
			break
		}
	}
	if best == "" {
		return "", lastErr
	}
	return best, nil
}

// tgSendFile 把一个本地文件发给主人，caption 是一句纯文本说明（可空），replyTo > 0 时挂在那条消息下面。
// 太大发不了时改发一句带路径的话（不算失败）。sendPhoto/sendVideo 说参数不对（如截图太长，宽高比超过 20）时改按文件再发一次。
func (a *App) tgSendFile(c *tgCreds, path, caption string, replyTo int64) error {
	send, note, err := a.prepareMedia(path)
	if err != nil {
		return err
	}
	if note != "" {
		if caption != "" {
			note = caption + "\n" + note
		}
		return a.tgSend(c, esc(note), replyTo)
	}
	form := url.Values{"chat_id": {strconv.FormatInt(c.ChatID, 10)}}
	if caption != "" {
		form.Set("caption", truncRunes(caption, 1000))
	}
	if replyTo > 0 {
		form.Set("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyTo))
	}
	method, field := "sendDocument", "document"
	switch mediaKind(send) {
	case "photo":
		if fi, err := os.Stat(send); err == nil && fi.Size() <= tgPhotoMax {
			method, field = "sendPhoto", "photo"
		}
	case "video":
		method, field = "sendVideo", "video"
		form.Set("supports_streaming", "true")
	}
	err = a.tgUpload(c, method, field, send, form)
	var te *tgError
	if method != "sendDocument" && errors.As(err, &te) && te.Code == 400 {
		form.Del("supports_streaming")
		err = a.tgUpload(c, "sendDocument", "document", send, form)
	}
	return err
}

// notifyFile：agentq notify file <路径> [说明]，马上把文件发给主人（对话任务里回图片、录屏用，不进推送队列）。
// 在对话任务里执行时（环境变量 AGENTQ_JOB 是那个任务），挂在主人那条消息下面。
func (a *App) notifyFile(args []string) error {
	if len(args) < 1 || len(args) > 2 || strings.TrimSpace(args[0]) == "" {
		return errors.New("要写文件路径\n" + notifyUsage)
	}
	path, err := filepath.Abs(expandHome(a.Home, args[0]))
	if err != nil {
		return err
	}
	caption := ""
	if len(args) == 2 {
		caption = strings.TrimSpace(args[1])
	}
	c, err := a.telegramCreds()
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("没配 Telegram，发不了文件（agentq notify setup 配）；文件在 %s", path)
	}
	var replyTo int64
	if id := os.Getenv(jobEnv); id != "" {
		replyTo = a.chatReplyTo(&Job{ID: filepath.Base(id)})
	}
	if err := a.tgSendFile(c, path, caption, replyTo); err != nil {
		return fmt.Errorf("发送失败：%w", err)
	}
	a.logf("推送：把 %s 发给了主人", a.tilde(path))
	return nil
}
