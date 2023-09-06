package uploadassets

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/picosh/pico/db"
	"github.com/picosh/pico/shared"
	"github.com/picosh/pico/shared/storage"
	"github.com/picosh/pico/wish/cms/util"
	"github.com/picosh/pico/wish/send/utils"
)

type ctxUserKey struct{}
type ctxBucketKey struct{}
type ctxBucketQuotaKey struct{}
type ctxProjectKey struct{}

func getProject(s ssh.Session) *db.Project {
	v := s.Context().Value(ctxProjectKey{})
	if v == nil {
		return nil
	}
	project := s.Context().Value(ctxProjectKey{}).(*db.Project)
	return project
}

func getBucket(s ssh.Session) (storage.Bucket, error) {
	bucket := s.Context().Value(ctxBucketKey{}).(storage.Bucket)
	if bucket.Name == "" {
		return bucket, fmt.Errorf("bucket not set on `ssh.Context()` for connection")
	}
	return bucket, nil
}

func getBucketQuota(s ssh.Session) uint64 {
	return s.Context().Value(ctxBucketQuotaKey{}).(uint64)
}

func getUser(s ssh.Session) (*db.User, error) {
	user := s.Context().Value(ctxUserKey{}).(*db.User)
	if user == nil {
		return user, fmt.Errorf("user not set on `ssh.Context()` for connection")
	}
	return user, nil
}

type FileData struct {
	*utils.FileEntry
	Text        []byte
	User        *db.User
	Bucket      storage.Bucket
	BucketQuota uint64
}

type UploadAssetHandler struct {
	DBPool  db.DB
	Cfg     *shared.ConfigSite
	Storage storage.ObjectStorage
}

func NewUploadAssetHandler(dbpool db.DB, cfg *shared.ConfigSite, storage storage.ObjectStorage) *UploadAssetHandler {
	return &UploadAssetHandler{
		DBPool:  dbpool,
		Cfg:     cfg,
		Storage: storage,
	}
}

func (h *UploadAssetHandler) Read(s ssh.Session, entry *utils.FileEntry) (os.FileInfo, io.ReaderAt, error) {
	user, err := getUser(s)
	if err != nil {
		return nil, nil, err
	}

	fileInfo := &utils.VirtualFile{
		FName:    filepath.Base(entry.Filepath),
		FIsDir:   false,
		FSize:    int64(entry.Size),
		FModTime: time.Unix(entry.Mtime, 0),
	}

	bucket, err := h.Storage.GetBucket(shared.GetAssetBucketName(user.ID))
	if err != nil {
		return nil, nil, err
	}

	fname := shared.GetAssetFileName(entry)
	contents, err := h.Storage.GetFile(bucket, fname)
	if err != nil {
		return nil, nil, err
	}

	return fileInfo, contents, nil
}

func (h *UploadAssetHandler) List(s ssh.Session, fpath string) ([]os.FileInfo, error) {
	var fileList []os.FileInfo
	user, err := getUser(s)
	if err != nil {
		return fileList, err
	}
	cleanFilename := filepath.Base(fpath)
	bucketName := shared.GetAssetBucketName(user.ID)
	bucket, err := h.Storage.GetBucket(bucketName)
	if err != nil {
		return fileList, err
	}

	if cleanFilename == "" || cleanFilename == "." {
		name := cleanFilename
		if name == "" {
			name = "/"
		}

		info := &utils.VirtualFile{
			FName:  name,
			FIsDir: true,
		}
		fileList = append(fileList, info)
	} else {
		fileList, err = h.Storage.ListFiles(bucket, fpath, false)
		if err != nil {
			return fileList, err
		}
	}

	return fileList, nil
}

func (h *UploadAssetHandler) Validate(s ssh.Session) error {
	var err error
	key, err := util.KeyText(s)
	if err != nil {
		return fmt.Errorf("key not found")
	}

	user, err := h.DBPool.FindUserForKey(s.User(), key)
	if err != nil {
		return err
	}

	if user.Name == "" {
		return fmt.Errorf("must have username set")
	}

	if !h.DBPool.HasFeatureForUser(user.ID, "pgs") {
		return fmt.Errorf("you do not have access to this service")
	}

	assetBucket := shared.GetAssetBucketName(user.ID)
	bucket, err := h.Storage.UpsertBucket(assetBucket)
	if err != nil {
		return err
	}
	s.Context().SetValue(ctxBucketKey{}, bucket)

	totalFileSize, err := h.Storage.GetBucketQuota(bucket)
	if err != nil {
		return err
	}
	s.Context().SetValue(ctxBucketQuotaKey{}, totalFileSize)
	h.Cfg.Logger.Infof("(%s) bucket size is current (%d bytes)", user.Name, totalFileSize)

	s.Context().SetValue(ctxUserKey{}, user)
	h.Cfg.Logger.Infof("(%s) attempting to upload files to (%s)", user.Name, h.Cfg.Space)

	return nil
}

func (h *UploadAssetHandler) Write(s ssh.Session, entry *utils.FileEntry) (string, error) {
	user, err := getUser(s)
	if err != nil {
		return "", err
	}

	var origText []byte
	if b, err := io.ReadAll(entry.Reader); err == nil {
		origText = b
	}
	fileSize := binary.Size(origText)
	// TODO: hack for now until I figure out how to get correct
	// filesize from sftp,scp,rsync
	entry.Size = int64(fileSize)

	bucket, err := getBucket(s)
	if err != nil {
		return "", err
	}

	hasProject := getProject(s)
	projectName := shared.GetProjectName(entry)

	// find, create, or update project if we haven't already done it
	if hasProject == nil {
		project, err := h.DBPool.FindProjectByName(user.ID, projectName)
		if err == nil {
			err = h.DBPool.UpdateProject(user.ID, projectName)
			if err != nil {
				return "", err
			}
		} else {
			_, err = h.DBPool.InsertProject(user.ID, projectName, projectName)
			if err != nil {
				return "", err
			}
			project, err = h.DBPool.FindProjectByName(user.ID, projectName)
			if err != nil {
				return "", err
			}
		}
		s.Context().SetValue(ctxProjectKey{}, project)
	}

	bucketQuota := getBucketQuota(s)
	data := &FileData{
		FileEntry:   entry,
		User:        user,
		Text:        origText,
		Bucket:      bucket,
		BucketQuota: bucketQuota,
	}
	err = h.writeAsset(data)
	if err != nil {
		return "", err
	}

	url := h.Cfg.AssetURL(
		user.Name,
		projectName,
		strings.Replace(data.Filepath, "/"+projectName+"/", "", 1),
	)

	return url, nil
}
