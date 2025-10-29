/*
这些函数都是 Go 的单元测试。真正“执行”它们的是 `go test`。

当你在仓库里运行 `go test ./core/vm`（或更精确地 `go test ./core/vm -run TestSloadCache`）时，Go 的测试运行器会自动发现文件中以 `Test` 开头的函数，并依次调用它们。

函数本身不会主动输出结果；如果测试失败会通过 `t.Fatalf` 抛错，命令就会报告失败原因；所有断言都通过时，`go test` 只会给出成功的总结（如 `ok github.com/ethereum/go-ethereum/core/vm ...`）。
*/

package vm

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// countingState 嵌套包装 StateDB，记录额外的读取次数。
// 我们自定义的struct，包含StateDB，以及读取次数loads
type countingState struct {
	StateDB
	loads int
}

// GetState 输入: 合约地址、存储槽；输出: 存储值；作用: 统计并代理底层 GetState。
func (c *countingState) GetState(addr common.Address, key common.Hash) common.Hash {
	c.loads++
	return c.StateDB.GetState(addr, key)
}

// loadCount 输入: 无；输出: 当前累计读取次数；作用: 返回缓存命中统计值。
func (c *countingState) loadCount() int {
	return c.loads
}

// resetLoads 输入: 无；输出: 无；作用: 重置读取统计，方便单测独立断言。
func (c *countingState) resetLoads() {
	c.loads = 0
}

// newCountingState 输入: testing.T；输出: 包装过的 StateDB 以及原始 StateDB；作用: 提供可计数的状态对象。
func newCountingState(t *testing.T) (*countingState, *state.StateDB) {
	t.Helper()
	// 使用内存后端创建一份干净的状态树，避免外部副作用。
	caching := state.NewDatabaseForTesting()
	statedb, err := state.New(types.EmptyRootHash, caching)
	if err != nil {
		t.Fatalf("failed to create state: %v", err)
	}
	return &countingState{StateDB: statedb}, statedb
}

// newTestEVM 输入: StateDB；输出: *EVM；作用: 构造最小化上下文的测试 EVM 实例。
func newTestEVM(state StateDB) *EVM {
	blockCtx := BlockContext{
		CanTransfer: func(StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(StateDB, common.Address, common.Address, *uint256.Int) {},
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
	}
	return NewEVM(blockCtx, state, params.TestChainConfig, Config{})
}

// buildDoubleSloadCode 输入: 存储槽；输出: 字节码切片；作用: 生成连续两次 SLOAD 的合约代码。
func buildDoubleSloadCode(slot common.Hash) []byte {
	// 根据 SLOAD -> POP -> SLOAD -> STOP 顺序堆叠指令。
	code := make([]byte, 0, 1+32+3+1+32+2)
	code = append(code, 0x7f)
	code = append(code, slot.Bytes()...)
	code = append(code, 0x54)
	code = append(code, 0x50)
	code = append(code, 0x7f)
	code = append(code, slot.Bytes()...)
	code = append(code, 0x54)
	code = append(code, 0x00)
	return code
}

// buildSstoreThenSloadCode 输入: 存储槽、新值；输出: 字节码切片；作用: 生成先 SLOAD/POP 再 SSTORE/SLOAD 的代码路径。
func buildSstoreThenSloadCode(slot common.Hash, newValue common.Hash) []byte {
	// 组合指令以演示写入后再次读取会触发缓存失效。
	code := make([]byte, 0, (1+32+2)+(1+32+1)+(1+32+2))
	code = append(code, 0x7f)
	code = append(code, slot.Bytes()...)
	code = append(code, 0x54)
	code = append(code, 0x50)
	code = append(code, 0x7f)
	code = append(code, newValue.Bytes()...)
	code = append(code, 0x7f)
	code = append(code, slot.Bytes()...)
	code = append(code, 0x55)
	code = append(code, 0x7f)
	code = append(code, slot.Bytes()...)
	code = append(code, 0x54)
	code = append(code, 0x00)
	return code
}

// TestSloadCacheHitsWithinFrame 输入: testing.T；输出: 无；作用: 断言同一调用帧中重复 SLOAD 只访问底层一次。
func TestSloadCacheHitsWithinFrame(t *testing.T) {
	counting, underlying := newCountingState(t)

	contractAddr := common.HexToAddress("0x1000000000000000000000000000000000000001")
	slot := common.HexToHash("0x01")
	value := common.HexToHash("0x02")

	// 预置合约账户及初始存储，确保 SLOAD 能读取到数据。
	underlying.CreateAccount(contractAddr)
	underlying.SetState(contractAddr, slot, value)

	evm := newTestEVM(counting)
	// 显式创建缓存映射，以模拟真实调用帧的缓存生命周期。
	evm.sloadCache = make(map[sloadKey]common.Hash)

	caller := common.HexToAddress("0x2000000000000000000000000000000000000002")
	gas := uint64(1_000_000)
	callValue := new(uint256.Int)
	contract := NewContract(caller, contractAddr, callValue, gas, evm.jumpDests)

	code := buildDoubleSloadCode(slot)
	contract.SetCallCode(crypto.Keccak256Hash(code), code)

	counting.resetLoads()

	// 执行合约，期待内部只命中一次状态读取，第二次由缓存返回。
	if _, err := evm.Run(contract, nil, false); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	if got := counting.loadCount(); got != 1 {
		t.Fatalf("expected 1 storage load, got %d", got)
	}
}

// TestSloadCacheInvalidatedBySstore 输入: testing.T；输出: 无；作用: 验证写入操作会清除缓存并触发下一次真实读取。
func TestSloadCacheInvalidatedBySstore(t *testing.T) {
	counting, underlying := newCountingState(t)

	contractAddr := common.HexToAddress("0x3000000000000000000000000000000000000003")
	slot := common.HexToHash("0x04")
	initial := common.HexToHash("0x05")
	updated := common.HexToHash("0x06")

	underlying.CreateAccount(contractAddr)
	underlying.SetState(contractAddr, slot, initial)

	evm := newTestEVM(counting)
	evm.sloadCache = make(map[sloadKey]common.Hash)

	caller := common.HexToAddress("0x4000000000000000000000000000000000000004")
	gas := uint64(1_000_000)
	callValue := new(uint256.Int)
	contract := NewContract(caller, contractAddr, callValue, gas, evm.jumpDests)

	code := buildSstoreThenSloadCode(slot, updated)
	contract.SetCallCode(crypto.Keccak256Hash(code), code)

	counting.resetLoads()

	// 执行顺序包含 SSTORE，因此期望缓存被逐出，后续 SLOAD 会再次访问状态。
	if _, err := evm.Run(contract, nil, false); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	if got := counting.loadCount(); got != 2 {
		t.Fatalf("expected 2 storage loads, got %d", got)
	}

	if got := underlying.GetState(contractAddr, slot); got != updated {
		t.Fatalf("expected storage to be updated, got %x", got.Bytes())
	}
}
