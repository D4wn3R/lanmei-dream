package ai

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// visionSystemPrompt 视觉理解系统提示：要求模型以中文客观描述图片内容。
const visionSystemPrompt = "你是图片理解助手。请用简体中文客观、简洁地描述这张图片的内容（主体、场景、文字、氛围等），" +
	"控制在 200 字以内，不要猜测图片之外的信息。"

// VisionService 基于多模态 LLM 的图片理解服务。
// 降级链由调用方 MediaPass 负责：模型可用则返回文字描述；调用失败返回错误并由调用方回退
// "[图片]" 占位；未配置视觉模型（Vision=nil）时直接占位，不进入本服务。
type VisionService struct {
	model        model.BaseChatModel
	systemPrompt string
	timeout      time.Duration // 单次理解超时
	maxDescLen   int           // 描述最大长度（rune）
	logger       *zap.Logger
}

// NewVisionService 创建视觉理解服务。
// m 为支持多模态输入的 eino 模型（建议使用独立的视觉模型，不占用主对话模型配额）。
func NewVisionService(m model.BaseChatModel, logger *zap.Logger) *VisionService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &VisionService{
		model:        m,
		systemPrompt: visionSystemPrompt,
		timeout:      30 * time.Second,
		maxDescLen:   200,
		logger:       logger,
	}
}

// Describe 对图片执行视觉理解，返回中文文字描述。
// imageData 以 data URL 内嵌请求，无需对象存储公网可达（生产 RustFS 为内网地址，
// 预签名 URL 云端模型拉不到）。
// 不传思考开关选项：部分视觉模型（如 glm 系列）不支持 thinking.type=disabled，
// 传了会直接 400（thinking.type `disabled` is not supported by this model）。
func (v *VisionService) Describe(ctx context.Context, imageData []byte, mime string) (string, error) {
	if len(imageData) == 0 {
		return "", errors.New("vision: image data 为空")
	}
	if v.model == nil {
		return "", errors.New("vision: 模型未配置")
	}
	if mime == "" {
		mime = "image/png"
	}
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imageData)

	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	msgs := []*schema.Message{
		{Role: schema.System, Content: v.systemPrompt},
		{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{URL: &dataURL},
			}},
		}},
	}

	resp, err := v.model.Generate(ctx, msgs)
	if err != nil {
		return "", fmt.Errorf("vision: 图片理解失败: %w", err)
	}

	desc := strings.TrimSpace(resp.Content)
	if desc == "" {
		return "", errors.New("vision: 模型返回空描述")
	}

	// rune 级截断，避免切割 UTF-8 字符
        r := []rune(desc)
        if len(r) > v.maxDescLen {
                desc = string(r[:v.maxDescLen]) + "…"
        }
        v.logger.Debug("图片理解完成", zap.Int("len", len([]rune(desc))))
        return desc, nil
}


