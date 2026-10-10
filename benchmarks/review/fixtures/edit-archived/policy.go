// Package policy decides who may edit documents.
package policy

// User is an account.
type User struct {
	ID    string
	Admin bool
}

// Doc is a document.
type Doc struct {
	Owner    string
	Archived bool
}

// CanEdit reports whether u may edit d. Owners and admins may edit.
func CanEdit(u User, d Doc) bool {
	return u.Admin || u.ID == d.Owner
}
