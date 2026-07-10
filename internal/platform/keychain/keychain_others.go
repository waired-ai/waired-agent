//go:build !darwin

package keychain

type stubStore struct{}

func newStore() Store { return stubStore{} }

func (stubStore) Set(Item, []byte) error    { return ErrUnsupported }
func (stubStore) Get(Item) ([]byte, error)  { return nil, ErrUnsupported }
func (stubStore) Delete(Item) error         { return ErrUnsupported }
func (stubStore) Exists(Item) (bool, error) { return false, ErrUnsupported }
