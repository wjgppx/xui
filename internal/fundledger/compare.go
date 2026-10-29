package fundledger

import "sort"

// Compare matches only explicit references inside a selected account pair. It
// does not infer settlement from equal amounts, dates or corrected balances.
func Compare(merchant Document, bank Document) (Comparison, error) {
	if (merchant.Provider != "alipay" && merchant.Provider != "wechat") || bank.Provider != "bank" || merchant.AccountKey == bank.AccountKey {
		return Comparison{}, invalid(0, "provider", "invalid_pair")
	}
	if err := validateDocument(merchant); err != nil {
		return Comparison{}, err
	}
	if err := validateDocument(bank); err != nil {
		return Comparison{}, err
	}
	result := Comparison{Version: ComparisonVersion, MerchantAccountKey: merchant.AccountKey, BankAccountKey: bank.AccountKey, SourceWarnings: []string{}, Rows: []TransferComparison{}}
	if merchant.Coverage == "partial" {
		result.SourceWarnings = append(result.SourceWarnings, "merchant_partial")
	}
	if bank.Coverage == "partial" {
		result.SourceWarnings = append(result.SourceWarnings, "bank_partial")
	}
	if merchant.BalanceDifferenceCount > 0 || len(merchant.SummaryCodes) > 0 {
		result.SourceWarnings = append(result.SourceWarnings, "merchant_balance_issues")
	}
	if bank.BalanceDifferenceCount > 0 || len(bank.SummaryCodes) > 0 {
		result.SourceWarnings = append(result.SourceWarnings, "bank_balance_issues")
	}
	groups := map[string]*TransferComparison{}
	groupFor := func(id string) *TransferComparison {
		if group, exists := groups[id]; exists {
			return group
		}
		group := &TransferComparison{TransferID: id, MerchantEntries: []Entry{}, BankEntries: []Entry{}, Codes: []string{}}
		groups[id] = group
		return group
	}
	for _, row := range merchant.Rows {
		if !transferCandidate(merchant.Provider, row) {
			result.MerchantOtherCount++
		} else if row.CounterpartyAccountKey != bank.AccountKey {
			result.MerchantOutOfScopeCount++
		} else {
			result.MerchantCandidateCount++
			group := groupFor(row.TransferID)
			group.MerchantEntries = append(group.MerchantEntries, row)
		}
	}
	for _, row := range bank.Rows {
		if !transferCandidate(bank.Provider, row) {
			result.BankOtherCount++
		} else if row.CounterpartyAccountKey != merchant.AccountKey {
			result.BankOutOfScopeCount++
		} else {
			result.BankCandidateCount++
			group := groupFor(row.TransferID)
			group.BankEntries = append(group.BankEntries, row)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		switch {
		case len(group.MerchantEntries) > 1 || len(group.BankEntries) > 1:
			group.Status = "ambiguous"
			group.Codes = append(group.Codes, "duplicate_transfer_id")
			result.AmbiguousCount++
		case len(group.MerchantEntries) == 0:
			group.Status = "unmatched"
			group.Codes = append(group.Codes, "merchant_record_missing")
			result.UnmatchedCount++
		case len(group.BankEntries) == 0:
			group.Status = "unmatched"
			group.Codes = append(group.Codes, "bank_record_missing")
			result.UnmatchedCount++
		case group.MerchantEntries[0].AmountYuan != group.BankEntries[0].AmountYuan:
			group.Status = "difference"
			group.Codes = append(group.Codes, "amount_mismatch")
			result.DifferenceCount++
		default:
			group.Status = "matched"
			result.MatchedCount++
		}
		result.Rows = append(result.Rows, *group)
	}
	return result, nil
}
