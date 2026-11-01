package driver

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

type runtimeIdentity struct {
	uid        int
	gid        int
	home       string
	credential *syscall.Credential
}

// No sudo, login shell, environment inheritance, group grant, or account creation.
// Root Nomad drops UID/GID and uses only the account's existing groups for limactl.
func resolveRuntimeUser(name string) (*runtimeIdentity, error) {
	var u *user.User
	var e error
	if name == "" {
		u, e = user.Current()
	} else {
		u, e = user.Lookup(name)
	}
	if e != nil {
		return nil, e
	}
	uid, e := strconv.Atoi(u.Uid)
	if e != nil {
		return nil, e
	}
	gid, e := strconv.Atoi(u.Gid)
	if e != nil {
		return nil, e
	}
	if uid == 0 {
		return nil, fmt.Errorf("Lima cannot run as root: configure explicit existing unprivileged runtime_user")
	}
	if os.Geteuid() != 0 && uid != os.Geteuid() {
		return nil, fmt.Errorf("runtime_user must match current unprivileged driver identity")
	}
	r := &runtimeIdentity{uid: uid, gid: gid, home: u.HomeDir}
	if os.Geteuid() == 0 {
		groups, e := u.GroupIds()
		if e != nil {
			return nil, e
		}
		ids := make([]uint32, 0, len(groups))
		for _, v := range groups {
			n, e := strconv.ParseUint(v, 10, 32)
			if e != nil {
				return nil, e
			}
			ids = append(ids, uint32(n))
		}
		r.credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: ids}
	}
	return r, nil
}
func (r *runtimeIdentity) validateHome(path string) error {
	st, e := os.Stat(path)
	if e != nil {
		return e
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(raw.Uid) != r.uid {
		return fmt.Errorf("Lima home must already belong to runtime_user; driver does not change ownership")
	}
	return nil
}
