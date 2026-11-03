package secrets

// 测试注入点：替换 keyring 读写为内存实现。
// 仅供 *_test.go 使用（跨包测试也需注入会话缓存）。
var (
	keyringGet    = defaultKeyringGet
	keyringSet    = defaultKeyringSet
	keyringDelete = defaultKeyringDelete
)

// SetKeyringForTest 注入内存实现。
func SetKeyringForTest(get func(string) (string, error),
	set func(string, string) error, del func(string) error) {
	keyringGet = get
	keyringSet = set
	keyringDelete = del
}

// ResetKeyring 恢复真实 keyring。
func ResetKeyring() {
	keyringGet = defaultKeyringGet
	keyringSet = defaultKeyringSet
	keyringDelete = defaultKeyringDelete
}
