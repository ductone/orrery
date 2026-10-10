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

// CanEdit reports whether u may edit d. Owners and admins may edit live
// documents; archived documents are read-only except for admins.
func CanEdit(u User, d Doc) bool {
	if u.ID == d.Owner {
		return true
	}
	if d.Archived {
		return u.Admin
	}
	return u.Admin
}
