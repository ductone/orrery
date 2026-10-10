// Package bank keeps named account balances in memory.
package bank

import "errors"

var (
	// ErrNoAccount is returned for an unknown account name.
	ErrNoAccount = errors.New("bank: no such account")
	// ErrInsufficient is returned when an account lacks the funds.
	ErrInsufficient = errors.New("bank: insufficient funds")
)

// Bank holds accounts. Balances are whole cents and never negative.
type Bank struct {
	accounts map[string]int
}

// New returns an empty Bank.
func New() *Bank { return &Bank{accounts: map[string]int{}} }

// Open creates an account, or resets an existing one, with the given balance.
func (b *Bank) Open(name string, balance int) { b.accounts[name] = balance }

// Balance returns the balance of an account.
func (b *Bank) Balance(name string) (int, error) {
	bal, ok := b.accounts[name]
	if !ok {
		return 0, ErrNoAccount
	}
	return bal, nil
}

// Withdraw removes n cents from an account.
func (b *Bank) Withdraw(name string, n int) error {
	bal, ok := b.accounts[name]
	if !ok {
		return ErrNoAccount
	}
	if bal < n {
		return ErrInsufficient
	}
	b.accounts[name] = bal - n
	return nil
}

// Deposit adds n cents to an account.
func (b *Bank) Deposit(name string, n int) error {
	bal, ok := b.accounts[name]
	if !ok {
		return ErrNoAccount
	}
	b.accounts[name] = bal + n
	return nil
}

// Transfer moves n cents from one account to another. If it fails, neither
// balance changes. Transferring to the same account is allowed when the
// account holds at least n cents, and changes nothing.
func (b *Bank) Transfer(from, to string, n int) error {
	if err := b.Withdraw(from, n); err != nil {
		return err
	}
	if err := b.Deposit(to, n); err != nil {
		b.Deposit(from, n)
		return err
	}
	return nil
}

// Total returns the sum of all balances.
func (b *Bank) Total() int {
	sum := 0
	for _, bal := range b.accounts {
		sum += bal
	}
	return sum
}
