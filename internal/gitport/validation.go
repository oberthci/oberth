package gitport

import (
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func refusal(path, code, value, expected string) *AuthorRefusalError {
	message := fmt.Sprintf("Invalid Git %s %q.", path, value)
	if value == "" {
		message = "The Git " + path + " is missing."
	}
	return &AuthorRefusalError{Path: path, Code: code, Message: message, Hint: "Use " + expected + "."}
}
func validateRepository(value string) error {
	if _, err := reponame.ParseRepository(value); err != nil {
		return refusal("repository", "invalid_repository", value, "a canonical repository such as origin/team/widget")
	}
	return nil
}
func validateObject(field, value string) error {
	if !gitoid.Valid(value) {
		return refusal(field, "invalid_object", value, "a full lowercase 40- or 64-character object ID such as "+strings.Repeat("a", 40))
	}
	return nil
}
func validateRevision(r Revision) error {
	if err := validateRepository(r.Repository); err != nil {
		return err
	}
	return validateObject("commit", r.Commit)
}
func validateRef(repository string, ref Ref) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		failure := refusal("ref", "invalid_ref", ref.Name, "a public ref such as refs/heads/main with full lowercase object/commit IDs; a tag may omit its peeled commit")
		var invalid *sourcecontract.InvalidError
		if errors.As(err, &invalid) {
			failure.Message += " " + invalid.Path() + ": " + invalid.Reason()
		}
		if ref.ObjectSHA != "" {
			failure.Message += fmt.Sprintf(" Object ID: %q.", ref.ObjectSHA)
		}
		if ref.CommitSHA != "" {
			failure.Message += fmt.Sprintf(" Commit ID: %q.", ref.CommitSHA)
		}
		return failure
	}
	return nil
}
func validateFilePath(value string) error {
	if err := sourcecontract.ValidateFilePath(value); err != nil {
		return refusal("file", "invalid_path", value, "a clean relative file path such as .oberth/build.yaml")
	}
	return nil
}

func (r PromotionRequest) Validate() error {
	if r.ActionReceipt == "" {
		return refusal("action receipt", "missing_action", r.ActionReceipt, "an attributed action receipt")
	}
	if err := validateTarget(r.Repository, r.Target); err != nil {
		return err
	}
	if err := validateRevision(r.Source); err != nil {
		return err
	}
	if r.Source.Repository != r.Repository {
		return refusal("source repository", "invalid_source", r.Source.Repository, "a tested revision from the target repository")
	}
	return nil
}
func validateTarget(repository string, ref Ref) error {
	if err := validateRepository(repository); err != nil {
		return err
	}
	if err := sourcecontract.ValidatePublicRef(ref.Name); err != nil {
		return refusal("target", "invalid_ref", ref.Name, "an exact branch ref such as refs/heads/main")
	}
	if !strings.HasPrefix(ref.Name, "refs/heads/") {
		return refusal("target", "invalid_ref", ref.Name, "an exact branch ref such as refs/heads/main")
	}
	if ref.ObjectSHA == "" && ref.CommitSHA == "" {
		return nil
	}
	return validateRef(repository, ref)
}
func (f File) Validate(revision Revision) error {
	if err := validateRevision(revision); err != nil {
		return err
	}
	if err := validateFilePath(f.Path); err != nil {
		return err
	}
	if f.Mode != 0100644 && f.Mode != 0100755 {
		return refusal("file mode", "not_regular", fmt.Sprintf("%06o", f.Mode), "a regular Git file mode, 100644 or 100755")
	}
	if len(f.Bytes) > sourcecontract.MaxFileBytes {
		return refusal("file bytes", "too_large", fmt.Sprint(len(f.Bytes)), "a selected file of at most 1 MiB")
	}
	return validateObject("file blob", f.Blob)
}
func (p Promotion) Validate() error {
	if err := p.Request.Validate(); err != nil {
		return err
	}
	if len(p.Conflicts) > 0 {
		if p.Result != (Ref{}) || p.Tree != "" {
			return &AuthorRefusalError{Path: "promotion", Code: "conflicting_result", Message: "The promotion has conflicts and also contains a publishable result.", Hint: "Resolve the conflicts before publishing a merge result."}
		}
		for _, name := range p.Conflicts {
			if err := validateFilePath(name); err != nil {
				return err
			}
		}
		return nil
	}
	if p.Result.Name != p.Request.Target.Name {
		failure := refusal("promotion result", "wrong_target", p.Result.Name, "the requested target branch")
		failure.Message += fmt.Sprintf(" Requested target: %q.", p.Request.Target.Name)
		return failure
	}
	if err := validateRef(p.Request.Repository, p.Result); err != nil {
		return err
	}
	return validateObject("result tree", p.Tree)
}
