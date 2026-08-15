// Package splitbill 实现费用分摊结算的核心业务逻辑：根据分摊模式计算每笔账单
// 各参与者的份额、累计成员净额，并给出使净额清零的结算转账方案。
// 全程使用整数（分）运算，不引入浮点，不依赖第三方库。
package splitbill

import (
	"errors"
	"sort"
)

// Mode 分摊模式。
type Mode string

const (
	// ModeEqual 等额分摊：金额在全部参与者之间平均分配。
	ModeEqual Mode = "equal"
	// ModeRatio 按比例分摊：按各参与者权重比例分配。
	ModeRatio Mode = "ratio"
	// ModeFixed 按固定额分摊：部分参与者带固定金额，其余参与者分摊差额。
	ModeFixed Mode = "fixed"
)

// 校验与计算过程中可能返回的错误。每个错误对应一种独立的失败原因，
// 调用方可通过 errors.Is 精确区分，而不必解析字符串。
var (
	ErrAmountNotPositive   = errors.New("splitbill: 账单金额必须为正整数分")
	ErrEmptyParticipants   = errors.New("splitbill: 参与者列表不能为空")
	ErrUnknownMode         = errors.New("splitbill: 未知的分摊模式")
	ErrRatioWeightsInvalid = errors.New("splitbill: 按比例分摊权重必须为非负整数且至少一个为正")
	ErrFixedNegative       = errors.New("splitbill: 固定额必须为非负整数")
	ErrFixedExceedsTotal   = errors.New("splitbill: 固定额之和大于账单总额")
	ErrFixedNoFreeRemain   = errors.New("splitbill: 固定额之和小于账单总额但无参与者分摊差额")
	ErrNegativeShare       = errors.New("splitbill: 分摊份额不得为负")
	ErrShareSumMismatch    = errors.New("splitbill: 分摊份额之和与账单总额不一致")
)

// ParticipantInput 请求中单个参与者的描述。
// Weight 仅在 ratio 模式下使用；Fixed 仅在 fixed 模式下使用，
// Fixed 为 nil 表示该参与者未指定固定额（即“自由参与者”）。
type ParticipantInput struct {
	Name   string
	Weight int64
	Fixed  *int64
}

// Share 某参与者在某笔账单中的分摊结果。
type Share struct {
	Name        string
	AmountCents int64
}

// Transfer 结算转账方案中的一笔转账：From 向 To 支付 AmountCents 分。
type Transfer struct {
	From        string
	To          string
	AmountCents int64
}

// ComputeShares 根据账单金额、分摊模式与参与者列表计算各参与者的分摊份额。
// 返回的份额顺序与 participants 中实际参与分摊者的出现顺序一致；
// 对份额为 0 但仍列出的参与者（如 fixed 模式下固定额为 0、ratio 模式下权重为 0），
// 也按其出现顺序返回对应 0 份额项。份额之和严格等于 amount。
func ComputeShares(amount int64, mode Mode, participants []ParticipantInput) ([]Share, error) {
	if amount <= 0 {
		return nil, ErrAmountNotPositive
	}
	if len(participants) == 0 {
		return nil, ErrEmptyParticipants
	}
	switch mode {
	case ModeEqual:
		return computeEqual(amount, participants), nil
	case ModeRatio:
		return computeRatio(amount, participants)
	case ModeFixed:
		return computeFixed(amount, participants)
	default:
		return nil, ErrUnknownMode
	}
}

// computeEqual 等额分摊：所有参与者权重视为 1，按 floor 取整后补差。
func computeEqual(amount int64, participants []ParticipantInput) []Share {
	weights := make([]int64, len(participants))
	for i := range participants {
		weights[i] = 1
	}
	raw := splitFloor(amount, weights)
	shares := make([]Share, len(participants))
	for i, p := range participants {
		shares[i] = Share{Name: p.Name, AmountCents: raw[i]}
	}
	return shares
}

// computeRatio 按比例分摊：权重为 0 的参与者份额为 0 且不参与补差。
func computeRatio(amount int64, participants []ParticipantInput) ([]Share, error) {
	weights := make([]int64, len(participants))
	for i, p := range participants {
		if p.Weight < 0 {
			return nil, ErrRatioWeightsInvalid
		}
		weights[i] = p.Weight
	}
	if !anyPositive(weights) {
		return nil, ErrRatioWeightsInvalid
	}
	// 只对权重大于 0 的参与者做 floor 与补差，保持其原始出现顺序。
	type idxWeight struct {
		idx     int
		weight  int64
	}
	var active []idxWeight
	for i, w := range weights {
		if w > 0 {
			active = append(active, idxWeight{idx: i, weight: w})
		}
	}
	activeWeights := make([]int64, len(active))
	for j, a := range active {
		activeWeights[j] = a.weight
	}
	activeRaw := splitFloor(amount, activeWeights)
	shares := make([]Share, len(participants))
	for i, p := range participants {
		shares[i] = Share{Name: p.Name, AmountCents: 0}
	}
	for j, a := range active {
		shares[a.idx].AmountCents = activeRaw[j]
	}
	return shares, nil
}

// computeFixed 按固定额分摊：处理恰好、差额等额补、超额拒绝三态。
func computeFixed(amount int64, participants []ParticipantInput) ([]Share, error) {
	shares := make([]Share, len(participants))
	var fixedSum int64
	var freeIndices []int
	for i, p := range participants {
		if p.Fixed != nil {
			if *p.Fixed < 0 {
				return nil, ErrFixedNegative
			}
			shares[i] = Share{Name: p.Name, AmountCents: *p.Fixed}
			fixedSum += *p.Fixed
		} else {
			shares[i] = Share{Name: p.Name, AmountCents: 0}
			freeIndices = append(freeIndices, i)
		}
	}

	switch {
	case fixedSum == amount:
		// 恰好：自由参与者份额保持 0。
		return shares, nil
	case fixedSum < amount:
		// 差额：在自由参与者中等额分摊（含补差）。无自由参与者则拒绝。
		if len(freeIndices) == 0 {
			return nil, ErrFixedNoFreeRemain
		}
		remainder := amount - fixedSum
		freeWeights := make([]int64, len(freeIndices))
		for j := range freeIndices {
			freeWeights[j] = 1
		}
		freeRaw := splitFloor(remainder, freeWeights)
		for j, idx := range freeIndices {
			shares[idx].AmountCents = freeRaw[j]
		}
		return shares, nil
	default:
		// fixedSum > amount：拒绝。
		return nil, ErrFixedExceedsTotal
	}
}

// splitFloor 按 weights 比例把 total 分成整数份额：先对每项做 floor，
// 再把累计差额（total 减去已分配之和）按顺序依次给每人补 1 分。
// 调用方需保证 weights 各项非负且至少一个为正、total 为正。
// 返回的份额之和严格等于 total。
func splitFloor(total int64, weights []int64) []int64 {
	n := len(weights)
	shares := make([]int64, n)
	var sumW int64
	for _, w := range weights {
		sumW += w
	}
	var assigned int64
	for i, w := range weights {
		shares[i] = total * w / sumW
		assigned += shares[i]
	}
	remainder := total - assigned
	// remainder 严格小于 n（floor 至多每项丢不足 1 分），按顺序补齐。
	for i := 0; i < n && remainder > 0; i++ {
		shares[i]++
		remainder--
	}
	return shares
}

// anyPositive 判断权重切片中是否存在正值。
func anyPositive(weights []int64) bool {
	for _, w := range weights {
		if w > 0 {
			return true
		}
	}
	return false
}

// ValidateShares 检查份额不变量：无负份额且之和等于 amount。
// 供调用方在记录账单前做最终防御性校验。
func ValidateShares(amount int64, shares []Share) error {
	var sum int64
	for _, s := range shares {
		if s.AmountCents < 0 {
			return ErrNegativeShare
		}
		sum += s.AmountCents
	}
	if sum != amount {
		return ErrShareSumMismatch
	}
	return nil
}

// NetBalance 根据已记录的账单计算每个成员的净额（已付金额减去应承担份额）。
// 返回的 map 包含 members 中的全部成员，即便净额为 0。
func NetBalance(members []string, bills []Bill) map[string]int64 {
	bal := make(map[string]int64, len(members))
	for _, m := range members {
		bal[m] = 0
	}
	for _, b := range bills {
		bal[b.Payer] += b.AmountCents
		for _, s := range b.Shares {
			bal[s.Name] -= s.AmountCents
		}
	}
	return bal
}

// Settle 在给定净额基础上计算使所有净额清零的转账方案。
// 采用确定性贪心：每步选取当前净额绝对值最大的债权人与债务人配对，
// 并列时按成员名称字典序升序选取；不修改传入的 balances。
// 返回的每笔转账金额为正整数分，净额为 0 的成员不出现在结果中。
func Settle(balances map[string]int64) []Transfer {
	// 复制一份，避免修改调用方的 map。
	bal := make(map[string]int64, len(balances))
	for k, v := range balances {
		bal[k] = v
	}
	var transfers []Transfer
	for {
		creditor, debtor := pickMaxPair(bal)
		if creditor == "" || debtor == "" {
			break
		}
		amt := bal[creditor]
		if d := -bal[debtor]; d < amt {
			amt = d
		}
		transfers = append(transfers, Transfer{From: debtor, To: creditor, AmountCents: amt})
		bal[creditor] -= amt
		bal[debtor] += amt
	}
	return transfers
}

// pickMaxPair 从净额中选出当前绝对值最大的债权人（净额>0）与债务人（净额<0）。
// 并列时按成员名称字典序升序。任一类不存在时对应返回空串。
// 遍历顺序不影响结果：比较条件已保证唯一确定性。
func pickMaxPair(bal map[string]int64) (creditor, debtor string) {
	// 对键排序以保证遍历稳定，便于阅读；比较逻辑本身已确定。
	names := make([]string, 0, len(bal))
	for k := range bal {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		b := bal[name]
		if b > 0 {
			if creditor == "" || b > bal[creditor] || (b == bal[creditor] && name < creditor) {
				creditor = name
			}
		} else if b < 0 {
			abs := -b
			curAbs := -bal[debtor] // debtor=="" 时为 0
			if debtor == "" || abs > curAbs || (abs == curAbs && name < debtor) {
				debtor = name
			}
		}
	}
	return creditor, debtor
}
