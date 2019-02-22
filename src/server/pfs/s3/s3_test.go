package s3

// Tests for the PFS' S3 emulation API. Note that, in calls to
// `tu.UniqueString`, all lowercase characters are used, unlike in other
// tests. This is in order to generate repo names that are also valid bucket
// names. Otherwise minio complains that the bucket name is not valid.

import (
	"fmt"
	// "io"
	"io/ioutil"
	// "os"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	minio "github.com/minio/minio-go"
	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pkg/require"
	"github.com/pachyderm/pachyderm/src/server/pfs/server"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	tu "github.com/pachyderm/pachyderm/src/server/pkg/testutil"
)

func serve(t *testing.T, pc *client.APIClient) (*http.Server, *minio.Client) {
	t.Helper()

	port := tu.UniquePort()
	srv := Server(pc, port)

	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			t.Fatalf("http server returned an error: %v", err)
		}
	}()

	// Wait for the server to start
	require.NoError(t, backoff.Retry(func() error {
		c := &http.Client{}
		res, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return err
		} else if res.StatusCode != 200 {
			return fmt.Errorf("Unexpected status code: %d", res.StatusCode)
		}
		return nil
	}, backoff.NewTestingBackOff()))

	c, err := minio.New(fmt.Sprintf("127.0.0.1:%d", port), "id", "secret", false)
	require.NoError(t, err)
	return srv, c
}

func getObject(t *testing.T, c *minio.Client, repo, branch, file string) (string, error) {
	t.Helper()

	obj, err := c.GetObject(repo, fmt.Sprintf("%s/%s", branch, file))
	if err != nil {
		return "", err
	}
	defer func() { err = obj.Close() }()
	bytes, err := ioutil.ReadAll(obj)
	if err != nil {
		return "", err
	}
	return string(bytes), err
}

func checkListObjects(t *testing.T, ch <-chan minio.ObjectInfo, startTime time.Time, endTime time.Time, expectedFiles []string) {
	t.Helper()

	// sort expectedFiles, as the S3 gateway should always return results in
	// sorted order
	sort.Strings(expectedFiles)

	objs := []minio.ObjectInfo{}
	for obj := range ch {
		objs = append(objs, obj)
	}

	require.Equal(t, len(expectedFiles), len(objs))

	for i := 0; i < len(expectedFiles); i++ {
		expectedFilename := expectedFiles[i]
		obj := objs[i]
		require.Equal(t, expectedFilename, obj.Key)
		require.Equal(t, "", obj.ETag, fmt.Sprintf("unexpected etag for %s", expectedFilename))

		if strings.HasSuffix(expectedFilename, "/") {
			// expected file is a dir
			require.Equal(t, int64(0), obj.Size)
			require.True(t, obj.LastModified.IsZero(), fmt.Sprintf("unexpected last modified for %s: %v", expectedFilename, obj.LastModified))

		} else {
			// expected file is a file
			expectedLen := int64(len(filepath.Base(expectedFilename)) + 1)
			require.Equal(t, expectedLen, obj.Size, fmt.Sprintf("unexpected file length for %s", expectedFilename))
			require.True(t, startTime.Before(obj.LastModified), fmt.Sprintf("unexpected last modified for %s", expectedFilename))
			require.True(t, endTime.After(obj.LastModified), fmt.Sprintf("unexpected last modified for %s", expectedFilename))
		}
	}
}

func nonServerError(t *testing.T, err error) {
	t.Helper()
	require.YesError(t, err)
	require.NotEqual(t, "500 Internal Server Error", err.Error(), "expected a non-500 error")
}

func TestListBuckets(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	startTime := time.Now()
	repo1 := tu.UniqueString("testlistbuckets1")
	require.NoError(t, pc.CreateRepo(repo1))
	repo2 := tu.UniqueString("testlistbuckets2")
	require.NoError(t, pc.CreateRepo(repo2))
	endTime := time.Now()

	buckets, err := c.ListBuckets()
	require.NoError(t, err)
	require.Equal(t, 2, len(buckets))

	for _, bucket := range buckets {
		require.EqualOneOf(t, []string{repo1, repo2}, bucket.Name)
		require.True(t, startTime.Before(bucket.CreationDate))
		require.True(t, endTime.After(bucket.CreationDate))
	}

	require.NoError(t, srv.Close())
}

func TestGetObject(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo := tu.UniqueString("testgetobject")
	require.NoError(t, pc.CreateRepo(repo))
	_, err := pc.PutFile(repo, "master", "file", strings.NewReader("content"))
	require.NoError(t, err)

	fetchedContent, err := getObject(t, c, repo, "master", "file")
	require.NoError(t, err)
	require.Equal(t, "content", fetchedContent)

	require.NoError(t, srv.Close())
}

func TestGetObjectInBranch(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo := tu.UniqueString("testgetobjectinbranch")
	require.NoError(t, pc.CreateRepo(repo))
	require.NoError(t, pc.CreateBranch(repo, "branch", "", nil))
	_, err := pc.PutFile(repo, "branch", "file", strings.NewReader("content"))
	require.NoError(t, err)

	fetchedContent, err := getObject(t, c, repo, "branch", "file")
	require.NoError(t, err)
	require.Equal(t, "content", fetchedContent)

	require.NoError(t, srv.Close())
}

func TestStatObject(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo := tu.UniqueString("teststatobject")
	require.NoError(t, pc.CreateRepo(repo))
	_, err := pc.PutFile(repo, "master", "file", strings.NewReader("content"))
	require.NoError(t, err)

	// `startTime` and `endTime` will be used to ensure that an object's
	// `LastModified` date is correct. A few minutes are subtracted/added to
	// each to tolerate the node time not being the same as the host time.
	startTime := time.Now().Add(time.Duration(-5) * time.Minute)
	_, err = pc.PutFileOverwrite(repo, "master", "file", strings.NewReader("new-content"), 0)
	require.NoError(t, err)
	endTime := time.Now().Add(time.Duration(5) * time.Minute)

	info, err := c.StatObject(repo, "master/file")
	require.NoError(t, err)
	require.True(t, startTime.Before(info.LastModified))
	require.True(t, endTime.After(info.LastModified))
	require.Equal(t, "", info.ETag) //etags aren't returned by our API
	require.Equal(t, "text/plain; charset=utf-8", info.ContentType)
	require.Equal(t, int64(11), info.Size)

	require.NoError(t, srv.Close())
}

func TestPutObject(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo := tu.UniqueString("testputobject")
	require.NoError(t, pc.CreateRepo(repo))
	require.NoError(t, pc.CreateBranch(repo, "branch", "", nil))

	_, err := c.PutObject(repo, "branch/file", strings.NewReader("content1"), "text/plain")
	require.NoError(t, err)

	// this should act as a PFS PutFileOverwrite
	_, err = c.PutObject(repo, "branch/file", strings.NewReader("content2"), "text/plain")
	require.NoError(t, err)

	fetchedContent, err := getObject(t, c, repo, "branch", "file")
	require.NoError(t, err)
	require.Equal(t, "content2", fetchedContent)

	require.NoError(t, srv.Close())
}

func TestRemoveObject(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo := tu.UniqueString("testremoveobject")
	require.NoError(t, pc.CreateRepo(repo))
	_, err := pc.PutFile(repo, "master", "file", strings.NewReader("content"))
	require.NoError(t, err)

	// as per PFS semantics, the second delete should be a no-op
	require.NoError(t, c.RemoveObject(repo, "master/file"))
	require.NoError(t, c.RemoveObject(repo, "master/file"))

	require.NoError(t, srv.Close())
}

// // Tests inserting and getting files over 64mb in size
// func TestLargeObjects(t *testing.T) {
// 	log.SetLevel(log.DebugLevel)

// 	pc := server.GetPachClient(t)
// 	srv, c := serve(t, pc)

// 	// test repos: repo1 exists, repo2 does not
// 	repo1 := tu.UniqueString("testlargeobject1")
// 	repo2 := tu.UniqueString("testlargeobject2")
// 	require.NoError(t, pc.CreateRepo(repo1))

// 	// create a temporary file to put 100mb of contents into it
// 	bytesWritten := 0
// 	inputFile, err := ioutil.TempFile("", "pachyderm-test-large-objects-input-*")
// 	require.NoError(t, err)
// 	defer os.Remove(inputFile.Name())
// 	for i := 0; i<2097152; i++ {
// 		n, err := inputFile.WriteString("no tv and no beer make homer something something.\n")
// 		require.NoError(t, err)
// 		bytesWritten += n
// 	}

// 	// make sure we wrote at least 65mb
// 	if bytesWritten < 68157440 {
// 		t.Errorf("too few bytes written to %s: %d", inputFile.Name(), bytesWritten)
// 	}

// 	// first ensure that putting into a repo that doesn't exist triggers an
// 	// error
// 	_, err = c.FPutObject(repo2, "file", inputFile.Name(), "text/plain")
// 	nonServerError(t, err)

// 	// now try putting into a legit repo
// 	l, err := c.FPutObject(repo1, "file", inputFile.Name(), "text/plain")
// 	require.NoError(t, err)
// 	require.Equal(t, bytesWritten, l)

// 	// create a file to write the results back to
// 	outputFile, err := ioutil.TempFile("", "pachyderm-test-large-objects-output-*")
// 	require.NoError(t, err)
// 	defer os.Remove(outputFile.Name())

// 	// try getting an object that does not exist
// 	err = c.FGetObject(repo2, "file", outputFile.Name())
// 	nonServerError(t, err)
// 	bytes, err := ioutil.ReadFile(outputFile.Name())
// 	require.NoError(t, err)
// 	require.Equal(t, 0, len(bytes))

// 	// get the file that does exist
// 	err = c.FGetObject(repo1, "file", outputFile.Name())
// 	require.NoError(t, err)

// 	// compare the input file and output file to ensure they're the same
// 	b1 := make([]byte, 65536)
// 	b2 := make([]byte, 65536)
// 	inputFile.Seek(0, 0)
// 	outputFile.Seek(0, 0)
// 	for {
// 		n1, err1 := inputFile.Read(b1)
// 		n2, err2 := outputFile.Read(b2)

// 		require.Equal(t, n1, n2)

// 		if err1 == io.EOF && err2 == io.EOF {
// 			break;
// 		}

// 		require.NoError(t, err1)
// 		require.NoError(t, err2)
// 		require.Equal(t, b1, b2)
// 	}

// 	require.NoError(t, srv.Close())
// }

func TestGetObjectNoHead(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo := tu.UniqueString("testgetobjectnohead")
	require.NoError(t, pc.CreateRepo(repo))
	require.NoError(t, pc.CreateBranch(repo, "branch", "", nil))

	_, err := getObject(t, c, repo, "branch", "file")
	nonServerError(t, err)

	require.NoError(t, srv.Close())
}

func TestGetObjectNoBranch(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo := tu.UniqueString("testgetobjectnobranch")
	require.NoError(t, pc.CreateRepo(repo))

	_, err := getObject(t, c, repo, "branch", "file")
	nonServerError(t, err)
	require.Equal(t, err.Error(), "The specified key does not exist.")
	require.NoError(t, srv.Close())
}

func TestGetObjectNoRepo(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo := tu.UniqueString("testgetobjectnorepo")
	_, err := getObject(t, c, repo, "master", "file")
	nonServerError(t, err)
	require.Equal(t, err.Error(), "The specified bucket does not exist.")
	require.NoError(t, srv.Close())
}

func TestMakeBucket(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)
	repo := tu.UniqueString("testmakebucket")
	require.NoError(t, c.MakeBucket(repo, ""))
	_, err := pc.InspectRepo(repo)
	require.NoError(t, err)
	require.NoError(t, srv.Close())
}

func TestMakeBucketWithRegion(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)
	repo := tu.UniqueString("testmakebucketwithregion")
	require.NoError(t, c.MakeBucket(repo, "us-east-1"))
	_, err := pc.InspectRepo(repo)
	require.NoError(t, err)
	require.NoError(t, srv.Close())
}

func TestMakeBucketRedundant(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)
	repo := tu.UniqueString("testmakebucketredundant")
	require.NoError(t, c.MakeBucket(repo, ""))
	nonServerError(t, c.MakeBucket(repo, ""))
	require.NoError(t, srv.Close())
}

func TestBucketExists(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo1 := tu.UniqueString("testbucketexists1")
	require.NoError(t, pc.CreateRepo(repo1))
	exists, err := c.BucketExists(repo1)
	require.NoError(t, err)
	require.True(t, exists)

	repo2 := tu.UniqueString("testbucketexists1")
	exists, err = c.BucketExists(repo2)
	require.NoError(t, err)
	require.False(t, exists)

	require.NoError(t, srv.Close())
}

func TestRemoveBucket(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	repo1 := tu.UniqueString("testremovebucket1")
	require.NoError(t, pc.CreateRepo(repo1))
	require.NoError(t, c.RemoveBucket(repo1))

	repo2 := tu.UniqueString("testremovebucket2")
	nonServerError(t, c.RemoveBucket(repo2))

	require.NoError(t, srv.Close())
}

func TestListObjects(t *testing.T) {
	pc := server.GetPachClient(t)
	srv, c := serve(t, pc)

	// create a bunch of files - enough to require the use of paginated
	// requests when browsing all files. One file will be included on a
	// separate branch to ensure it's not returned when querying against the
	// master branch. We also create a branch `emptybranch`. Because it has no
	// head commit, it should not show up when listing branches.
	// `startTime` and `endTime` will be used to ensure that an object's
	// `LastModified` date is correct. A few minutes are subtracted/added to
	// each to tolerate the node time not being the same as the host time.
	startTime := time.Now().Add(time.Duration(-5) * time.Minute)
	repo := tu.UniqueString("testlistobjects")
	require.NoError(t, pc.CreateRepo(repo))
	commit, err := pc.StartCommit(repo, "master")
	require.NoError(t, err)
	require.NoError(t, pc.CreateBranch(repo, "branch", "", nil))
	require.NoError(t, pc.CreateBranch(repo, "emptybranch", "", nil))
	for i := 0; i <= 1000; i++ {
		_, err = pc.PutFile(
			repo,
			commit.ID,
			fmt.Sprintf("%d", i),
			strings.NewReader(fmt.Sprintf("%d\n", i)),
		)
		require.NoError(t, err)
	}
	for i := 0; i < 10; i++ {
		_, err = pc.PutFile(
			repo,
			commit.ID,
			fmt.Sprintf("dir/%d", i),
			strings.NewReader(fmt.Sprintf("%d\n", i)),
		)
		require.NoError(t, err)
	}
	_, err = pc.PutFile(repo, "branch", "1001", strings.NewReader("1001\n"))
	require.NoError(t, pc.FinishCommit(repo, commit.ID))
	endTime := time.Now().Add(time.Duration(5) * time.Minute)

	// Request that will list branches as common prefixes
	ch := c.ListObjects(repo, "", false, make(chan struct{}))
	checkListObjects(t, ch, startTime, endTime, []string{"branch/", "master/"})
	ch = c.ListObjects(repo, "master", false, make(chan struct{}))
	checkListObjects(t, ch, startTime, endTime, []string{"master/"})

	// Request that will list all files in master's root
	ch = c.ListObjects(repo, "master/", false, make(chan struct{}))
	expectedFiles := []string{"master/dir/"}
	for i := 0; i <= 1000; i++ {
		expectedFiles = append(expectedFiles, fmt.Sprintf("master/%d", i))
	}
	checkListObjects(t, ch, startTime, endTime, expectedFiles)

	// Request that will list all files in master starting with 1
	ch = c.ListObjects(repo, "master/1", false, make(chan struct{}))
	expectedFiles = []string{}
	for i := 0; i <= 1000; i++ {
		file := fmt.Sprintf("master/%d", i)
		if strings.HasPrefix(file, "master/1") {
			expectedFiles = append(expectedFiles, file)
		}

	}
	checkListObjects(t, ch, startTime, endTime, expectedFiles)

	// Request that will list all files in a directory in master
	ch = c.ListObjects(repo, "master/dir/", false, make(chan struct{}))
	expectedFiles = []string{}
	for i := 0; i < 10; i++ {
		expectedFiles = append(expectedFiles, fmt.Sprintf("master/dir/%d", i))
	}
	checkListObjects(t, ch, startTime, endTime, expectedFiles)

	require.NoError(t, srv.Close())
}
