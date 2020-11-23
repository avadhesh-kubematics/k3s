// +build !no_embedded_executor

package executor

import (
	"github.com/rancher/k3s/pkg/version"
	"github.com/sirupsen/logrus"
	"go.etcd.io/etcd/embed"
	"go.etcd.io/etcd/etcdserver"
	"io/ioutil"
	"path/filepath"
	"strings"
)

func (e Embedded) CurrentETCDOptions() (InitialOptions, error) {
	return InitialOptions{}, nil
}

func (e Embedded) ETCD(args ETCDConfig) error {
	configFile, err := args.ToConfigFile()
	if err != nil {
		return err
	}
	cfg, err := embed.ConfigFromFile(configFile)
	if err != nil {
		return err
	}
	etcd, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil
	}

	go func() {
		select {
		case err := <-etcd.Server.ErrNotify():
			if strings.Contains(err.Error(), etcdserver.ErrMemberRemoved.Error()) {
				tombstoneFile := filepath.Join(args.DataDir, "tombstone")
				if err := ioutil.WriteFile(tombstoneFile, []byte{}, 0600); err != nil {
					logrus.Fatal("failed to write tombstone file to %s", tombstoneFile)
				}
				logrus.Infof("this node has been removed from the cluster please restart the %s to rejoin the cluster", version.Program)
				return
			//	if backupdatadir, err = util.BackupDirWithRetention(args.DataDir, 5); err != nil {
			//		logrus.Fatalf("Failed to remove old etcd datadir: %v", err)
			//	}
			}

		case <-etcd.Server.StopNotify():
			logrus.Fatalf("etcd stopped")
		case err := <-etcd.Err():
			logrus.Fatalf("etcd exited: %v", err)
		}
	}()
	return nil
}