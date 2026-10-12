package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Telegram 机器人：凭据只从 ~/.agentq/secrets/telegram.env 读（TELEGRAM_BOT_TOKEN、TELEGRAM_CHAT_ID，权限必须是 600），
// 不打印、不写进日志和状态文件，报错里出现的 token 一律打码。只给这一个 chat id 发消息，也只认它发来的消息。
// 接口见 core.telegram.org/bots/api：sendMessage 的 text 解析后 1–4096 字；HTML 模式里不属于标签的 < > & 必须转义；
// getUpdates 的 timeout 默认 0（短轮询），offset 比某条 update_id 大就算确认了它，没取的更新服务器最多留 24 小时；设了 webhook 时用不了。

type tgCreds struct {
	Token  string
	ChatID int64
}

func (a *App) telegramEnvPath() string { return filepath.Join(a.dataDir(), "secrets", "telegram.env") }

// telegramCreds：文件不存在返回 (nil, nil)，表示没配 Telegram（推送改用本机通知，也不收消息）；
// 存在但权限不是只有自己能读、缺键、chat id 不是数字时返回错误。
func (a *App) telegramCreds() (*tgCreds, error) {
	path := a.telegramEnvPath()
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s 的权限是 %o，别的用户也能读，先 chmod 600 再用", path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	tok, id := vals["TELEGRAM_BOT_TOKEN"], vals["TELEGRAM_CHAT_ID"]
	if tok == "" || id == "" {
		return nil, fmt.Errorf("%s 里缺 TELEGRAM_BOT_TOKEN 或 TELEGRAM_CHAT_ID（agentq notify setup 重新配）", path)
	}
	chat, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s 里的 TELEGRAM_CHAT_ID 不是数字", path)
	}
	return &tgCreds{Token: tok, ChatID: chat}, nil
}

var tokenRe = regexp.MustCompile(`bot\d+:[A-Za-z0-9_-]+`)

// redact 把文字里的 token 打码（Go 的 http 报错会带上完整 URL，URL 里有 token）。
func (c *tgCreds) redact(s string) string {
	if c != nil && c.Token != "" {
		s = strings.ReplaceAll(s, c.Token, "***")
	}
	return tokenRe.ReplaceAllString(s, "bot***")
}

// telegramAPI：环境变量 AGENTQ_TELEGRAM_API 可以换成自建的 Bot API 服务（测试里指向假服务；单元测试设 App.TelegramAPI）。
func (a *App) telegramAPI() string {
	if a.TelegramAPI != "" {
		return strings.TrimRight(a.TelegramAPI, "/")
	}
	if v := os.Getenv("AGENTQ_TELEGRAM_API"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.telegram.org"
}

type tgError struct {
	Code int
	Desc string
}

func (e *tgError) Error() string { return fmt.Sprintf("Telegram 报错 %d：%s", e.Code, e.Desc) }

func (a *App) tgCall(c *tgCreds, method string, form url.Values) (json.RawMessage, error) {
	return a.tgCallTimeout(c, method, form, 20*time.Second)
}

// tgCallTimeout：长轮询的 getUpdates 要等 timeout 秒才返回，HTTP 超时得比它长。
func (a *App) tgCallTimeout(c *tgCreds, method string, form url.Values, timeout time.Duration) (json.RawMessage, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.PostForm(a.telegramAPI()+"/bot"+c.Token+"/"+method, form)
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	return tgParse(c, method, resp)
}

func tgParse(c *tgCreds, method string, resp *http.Response) (json.RawMessage, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("Telegram %s 的返回看不懂（HTTP %d）：%s", method, resp.StatusCode, c.redact(oneLine(string(body), 200)))
	}
	if !r.OK {
		return nil, &tgError{Code: r.ErrorCode, Desc: c.redact(r.Description)}
	}
	return r.Result, nil
}

// tgMaxRunes：留出余量的单条消息长度上限（接口上限 4096 字，按解析后的字数算）。
const tgMaxRunes = 3800

// tgSend 发一条 HTML 消息，replyTo > 0 时作为对那条消息的回复。HTML 解析不了（400 can't parse entities）时
// 去掉标签按纯文本重发一次，免得一个没转义的尖括号让整条消息丢掉。
func (a *App) tgSend(c *tgCreds, text string, replyTo int64) error {
	form := url.Values{
		"chat_id":              {strconv.FormatInt(c.ChatID, 10)},
		"text":                 {text},
		"parse_mode":           {"HTML"},
		"link_preview_options": {`{"is_disabled":true}`},
	}
	if replyTo > 0 {
		form.Set("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyTo))
	}
	_, err := a.tgCall(c, "sendMessage", form)
	var te *tgError
	if errors.As(err, &te) && te.Code == 400 && strings.Contains(strings.ToLower(te.Desc), "parse") {
		form.Del("parse_mode")
		form.Set("text", htmlToPlain(text))
		_, err = a.tgCall(c, "sendMessage", form)
	}
	return err
}

type tgMsg struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	Caption   string `json:"caption"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	ReplyTo *tgMsg `json:"reply_to_message"`
	// 附件：photo 是同一张图的几种尺寸（最后一个最大）；同一组相册的几条消息 media_group_id 相同。
	Photo        []tgFile `json:"photo"`
	Video        *tgFile  `json:"video"`
	Document     *tgFile  `json:"document"`
	Animation    *tgFile  `json:"animation"`
	VideoNote    *tgFile  `json:"video_note"`
	Voice        *tgFile  `json:"voice"`
	Audio        *tgFile  `json:"audio"`
	MediaGroupID string   `json:"media_group_id"`
}

type tgFile struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FileName string `json:"file_name"`
}

type tgUpdate struct {
	UpdateID int64  `json:"update_id"`
	Message  *tgMsg `json:"message"`
}

// tgUpdates 取一次更新。wait 为 0 是短轮询（tick 兜底收消息时用，它拿着锁不能挂起）；
// 大于 0 是长轮询：没有新消息时服务器最多挂 wait 秒，有消息立刻返回（agentq listen 用）。
func (a *App) tgUpdates(c *tgCreds, offset int64, wait int) ([]tgUpdate, error) {
	form := url.Values{"timeout": {strconv.Itoa(wait)}, "limit": {"50"}, "allowed_updates": {`["message"]`}}
	if offset > 0 {
		form.Set("offset", strconv.FormatInt(offset, 10))
	}
	raw, err := a.tgCallTimeout(c, "getUpdates", form, time.Duration(wait)*time.Second+20*time.Second)
	if err != nil {
		return nil, err
	}
	var ups []tgUpdate
	if err := json.Unmarshal(raw, &ups); err != nil {
		return nil, fmt.Errorf("getUpdates 的结果看不懂：%v", err)
	}
	return ups, nil
}

// 官方上限（core.telegram.org/bots/api「Sending files」「getFile」）：机器人能下载的文件最大 20MB；
// multipart 上传图片最大 10MB、其他文件 50MB；sendPhoto 的宽高之和不超过 10000、宽高比不超过 20。
const (
	tgDownloadMax = 20 << 20
	tgPhotoMax    = 10 << 20
	tgUploadMax   = 50 << 20
	tgFileTimeout = 5 * time.Minute
)

// tgDownload 用 getFile 拿到文件路径再下载，存成 dest。超过 tgDownloadMax 的不下载（调用方先按 file_size 挡一次）。
// 返回 Telegram 那边的文件路径（带扩展名，没有文件名时用它补扩展名）。
func (a *App) tgDownload(c *tgCreds, fileID, dest string) (string, error) {
	raw, err := a.tgCall(c, "getFile", url.Values{"file_id": {fileID}})
	if err != nil {
		return "", err
	}
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.FilePath == "" {
		return "", fmt.Errorf("getFile 没给出文件路径")
	}
	if f.FileSize > tgDownloadMax {
		return f.FilePath, fmt.Errorf("文件 %s，超过机器人能下载的 20 MB", humanSize(f.FileSize))
	}
	client := &http.Client{Timeout: tgFileTimeout}
	resp, err := client.Get(a.telegramAPI() + "/file/bot" + c.Token + "/" + f.FilePath)
	if err != nil {
		return f.FilePath, errors.New(c.redact(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return f.FilePath, fmt.Errorf("下载文件 HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return f.FilePath, err
	}
	out, err := os.Create(dest)
	if err != nil {
		return f.FilePath, err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, tgDownloadMax+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > tgDownloadMax {
		err = fmt.Errorf("文件超过 20 MB")
	}
	if err != nil {
		os.Remove(dest)
		return f.FilePath, errors.New(c.redact(err.Error()))
	}
	return f.FilePath, nil
}

// tgUpload 用 multipart 上传一个本地文件（sendPhoto、sendVideo、sendDocument），field 是放文件的参数名。
func (a *App) tgUpload(c *tgCreds, method, field, path string, form url.Values) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var err error
		for k, vs := range form {
			for _, v := range vs {
				if err == nil {
					err = mw.WriteField(k, v)
				}
			}
		}
		if err == nil {
			var part io.Writer
			if part, err = mw.CreateFormFile(field, filepath.Base(path)); err == nil {
				_, err = io.Copy(part, f)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	client := &http.Client{Timeout: tgFileTimeout}
	resp, err := client.Post(a.telegramAPI()+"/bot"+c.Token+"/"+method, mw.FormDataContentType(), pr)
	if err != nil {
		pr.CloseWithError(err)
		return errors.New(c.redact(err.Error()))
	}
	_, err = tgParse(c, method, resp)
	return err
}

func humanSize(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

var tagRe = regexp.MustCompile(`<[^>]*>`)

// htmlToPlain 去掉 HTML 标签、还原实体，用于纯文本重发和本机通知。
func htmlToPlain(s string) string { return html.UnescapeString(tagRe.ReplaceAllString(s, "")) }

// esc 转义 HTML 模式里的 & < >（Telegram 只认这几个，加上 &quot;）。
func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
