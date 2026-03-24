package matching

import (
	"sync"
	"time"
)

const (
	// 时间戳位数
	timestampBits = 41
	// 机器ID位数
	machineIDBits = 10
	// 序列号位数
	sequenceBits = 12

	// 最大值
	maxMachineID = -1 ^ (-1 << machineIDBits) // 1023
	maxSequence  = -1 ^ (-1 << sequenceBits)  // 4095

	// 位移
	machineIDShift = sequenceBits
	timestampShift = sequenceBits + machineIDBits

	// 时间戳起始点 (2024-01-01 00:00:00 UTC)
	epoch = 1704067200000
)

// Snowflake Snowflake ID生成器
type Snowflake struct {
	mu         sync.Mutex
	machineID  uint64
	sequence   uint64
	lastTime   uint64
}

// NewSnowflake 创建Snowflake实例
func NewSnowflake(machineID uint64) (*Snowflake, error) {
	if machineID > maxMachineID {
		return nil, ErrInvalidMachineID
	}

	return &Snowflake{
		machineID: machineID,
	}, nil
}

// Generate 生成新的Snowflake ID
func (s *Snowflake) Generate() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := currentTimeMillis()

	// 时钟回拨检测
	if now < s.lastTime {
		return 0, ErrClockMovedBackwards
	}

	// 同一毫秒内，序列号递增
	if now == s.lastTime {
		s.sequence = (s.sequence + 1) & maxSequence
		// 序列号溢出，等待下一毫秒
		if s.sequence == 0 {
			now = s.waitNextMillis(now)
		}
	} else {
		// 不同毫秒，序列号重置
		s.sequence = 0
	}

	s.lastTime = now

	// 组装ID
	id := ((now - epoch) << timestampShift) |
		(s.machineID << machineIDShift) |
		s.sequence

	return id, nil
}

// waitNextMillis 等待下一毫秒
func (s *Snowflake) waitNextMillis(lastTime uint64) uint64 {
	now := currentTimeMillis()
	for now <= lastTime {
		now = currentTimeMillis()
	}
	return now
}

// currentTimeMillis 获取当前毫秒时间戳
func currentTimeMillis() uint64 {
	return uint64(time.Now().UnixMilli())
}

// ==========================================
// 全局Snowflake实例
// ==========================================

var (
	globalSnowflake *Snowflake
	snowflakeOnce   sync.Once
)

// InitGlobalSnowflake 初始化全局Snowflake实例
func InitGlobalSnowflake(machineID uint64) error {
	var err error
	snowflakeOnce.Do(func() {
		globalSnowflake, err = NewSnowflake(machineID)
	})
	return err
}

// GenerateTradeID 生成交易ID
func GenerateTradeID() string {
	if globalSnowflake == nil {
		// 如果未初始化，使用默认machineID=0
		InitGlobalSnowflake(0)
	}

	id, err := globalSnowflake.Generate()
	if err != nil {
		// 降级方案：使用时间戳
		return "t_" + formatTimestamp()
	}

	return "t_" + formatUint64(id)
}

// ==========================================
// 错误定义
// ==========================================

var (
	ErrInvalidMachineID    = &SnowflakeError{Code: 1, Message: "invalid machine ID"}
	ErrClockMovedBackwards = &SnowflakeError{Code: 2, Message: "clock moved backwards"}
)

// SnowflakeError Snowflake错误
type SnowflakeError struct {
	Code    int
	Message string
}

func (e *SnowflakeError) Error() string {
	return e.Message
}

// ==========================================
// 辅助函数
// ==========================================

func formatTimestamp() string {
	return time.Now().Format("20060102150405.000")
}

func formatUint64(n uint64) string {
	const charset = "0123456789abcdefghijklmnopqrstuvwxyz"
	var result [64]byte
	i := 64
	for {
		i--
		result[i] = charset[n%36]
		n /= 36
		if n == 0 {
			break
		}
	}
	return string(result[i:])
}
