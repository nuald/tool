package moby

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"path"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/opencontainers/runtime-spec/specs-go"
)

type tarWriter interface {
	Close() error
	Flush() error
	Write(b []byte) (n int, err error)
	WriteHeader(hdr *tar.Header) error
}

// This uses Docker to convert a Docker image into a tarball. It would be an improvement if we
// used the containerd libraries to do this instead locally direct from a local image
// cache as it would be much simpler.

// Unfortunately there are some files that Docker always makes appear in a running image and
// export shows them. In particular we have no way for a user to specify their own resolv.conf.
// Even if we were not using docker export to get the image, users of docker build cannot override
// the resolv.conf either, as it is not writeable and bind mounted in.

var exclude = map[string]bool{
	".dockerenv":   true,
	"Dockerfile":   true,
	"dev/console":  true,
	"dev/pts":      true,
	"dev/shm":      true,
	"etc/hostname": true,
}

var replace = map[string]string{
	"etc/hosts": `127.0.0.1       localhost
::1     localhost ip6-localhost ip6-loopback
fe00::0 ip6-localnet
ff00::0 ip6-mcastprefix
ff02::1 ip6-allnodes
ff02::2 ip6-allrouters
`,
	"etc/resolv.conf": `
# no resolv.conf configured
`,
}

// tarPrefix creates the leading directories for a path
func tarPrefix(path string, tw tarWriter) error {
	if path == "" {
		return nil
	}
	if path[len(path)-1] != byte('/') {
		return fmt.Errorf("path does not end with /: %s", path)
	}
	path = path[:len(path)-1]
	if path[0] == byte('/') {
		return fmt.Errorf("path should be relative: %s", path)
	}
	mkdir := ""
	for _, dir := range strings.Split(path, "/") {
		mkdir = mkdir + dir
		hdr := &tar.Header{
			Name:     mkdir,
			Mode:     0755,
			Typeflag: tar.TypeDir,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		mkdir = mkdir + "/"
	}
	return nil
}

// ImageTar takes a Docker image and outputs it to a tar stream
func ImageTar(image, prefix string, tw tarWriter, trust bool, pull bool, resolv string) error {
	log.Debugf("image tar: %s %s", image, prefix)
	if prefix != "" && prefix[len(prefix)-1] != byte('/') {
		return fmt.Errorf("prefix does not end with /: %s", prefix)
	}

	err := tarPrefix(prefix, tw)
	if err != nil {
		return err
	}

	if pull || trust {
		err := dockerPull(image, pull, trust)
		if err != nil {
			return fmt.Errorf("Could not pull image %s: %v", image, err)
		}
	}
	container, err := dockerCreate(image)
	if err != nil {
		// if the image wasn't found, pull it down.  Bail on other errors.
		if strings.Contains(err.Error(), "No such image") {
			err := dockerPull(image, true, trust)
			if err != nil {
				return fmt.Errorf("Could not pull image %s: %v", image, err)
			}
			container, err = dockerCreate(image)
			if err != nil {
				return fmt.Errorf("Failed to docker create image %s: %v", image, err)
			}
		} else {
			return fmt.Errorf("Failed to create docker image %s: %v", image, err)
		}
	}
	contents, err := dockerExport(container)
	if err != nil {
		return fmt.Errorf("Failed to docker export container from container %s: %v", container, err)
	}
	err = dockerRm(container)
	if err != nil {
		return fmt.Errorf("Failed to docker rm container %s: %v", container, err)
	}

	// now we need to filter out some files from the resulting tar archive

	r := bytes.NewReader(contents)
	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if exclude[hdr.Name] {
			log.Debugf("image tar: %s %s exclude %s", image, prefix, hdr.Name)
			_, err = io.Copy(ioutil.Discard, tr)
			if err != nil {
				return err
			}
		} else if replace[hdr.Name] != "" {
			if hdr.Name != "etc/resolv.conf" || resolv == "" {
				contents := replace[hdr.Name]
				hdr.Size = int64(len(contents))
				hdr.Name = prefix + hdr.Name
				log.Debugf("image tar: %s %s add %s", image, prefix, hdr.Name)
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				buf := bytes.NewBufferString(contents)
				_, err = io.Copy(tw, buf)
				if err != nil {
					return err
				}
			} else {
				// replace resolv.conf with specified symlink
				hdr.Name = prefix + hdr.Name
				hdr.Size = 0
				hdr.Typeflag = tar.TypeSymlink
				hdr.Linkname = resolv
				log.Debugf("image tar: %s %s add resolv symlink /etc/resolv.conf -> %s", image, prefix, resolv)
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
			}
			_, err = io.Copy(ioutil.Discard, tr)
			if err != nil {
				return err
			}
		} else {
			log.Debugf("image tar: %s %s add %s", image, prefix, hdr.Name)
			hdr.Name = prefix + hdr.Name
			if hdr.Typeflag == tar.TypeLink {
				// hard links are referenced by full path so need to be adjusted
				hdr.Linkname = prefix + hdr.Linkname
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			_, err = io.Copy(tw, tr)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// ImageBundle produces an OCI bundle at the given path in a tarball, given an image and a config.json
func ImageBundle(prefix string, image string, config []byte, runtime Runtime, tw tarWriter, trust bool, pull bool, readonly bool, dupMap map[string]string) error {
	// if read only, just unpack in rootfs/ but otherwise set up for overlay
	rootExtract := "rootfs"
	if !readonly {
		rootExtract = "lower"
	}

	// See if we have extracted this image previously
	root := path.Join(prefix, rootExtract)
	var foundElsewhere = dupMap[image] != ""
	if !foundElsewhere {
		if err := ImageTar(image, root+"/", tw, trust, pull, ""); err != nil {
			return err
		}
		dupMap[image] = root
	} else {
		if err := tarPrefix(prefix+"/", tw); err != nil {
			return err
		}
		root = dupMap[image]
	}

	hdr := &tar.Header{
		Name: path.Join(prefix, "config.json"),
		Mode: 0644,
		Size: int64(len(config)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	buf := bytes.NewBuffer(config)
	if _, err := io.Copy(tw, buf); err != nil {
		return err
	}

	if !readonly {
		// add a tmp directory to be used as a mount point for tmpfs for upper, work
		tmp := path.Join(prefix, "tmp")
		hdr = &tar.Header{
			Name:     tmp,
			Mode:     0755,
			Typeflag: tar.TypeDir,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		// add rootfs as merged mount point
		hdr = &tar.Header{
			Name:     path.Join(prefix, "rootfs"),
			Mode:     0755,
			Typeflag: tar.TypeDir,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		overlayOptions := []string{"lowerdir=/" + root, "upperdir=/" + path.Join(tmp, "upper"), "workdir=/" + path.Join(tmp, "work")}
		runtimeMounts := append(*runtime.Mounts,
			specs.Mount{Source: "tmpfs", Type: "tmpfs", Destination: "/" + tmp},
			// remount private as nothing else should see the temporary layers
			specs.Mount{Destination: "/" + tmp, Options: []string{"remount", "private"}},
			specs.Mount{Source: "overlay", Type: "overlay", Destination: "/" + path.Join(prefix, "rootfs"), Options: overlayOptions},
		)
		runtime.Mounts = &runtimeMounts
	} else {
		if foundElsewhere {
			// we need to make the mountpoint at rootfs
			hdr = &tar.Header{
				Name:     path.Join(prefix, "rootfs"),
				Mode:     0755,
				Typeflag: tar.TypeDir,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		}
		// either bind from another location, or bind from self to make sure it is a mountpoint as runc prefers this
		runtimeMounts := append(*runtime.Mounts, specs.Mount{Source: "/" + root, Destination: "/" + path.Join(prefix, "rootfs"), Options: []string{"bind"}})
		runtime.Mounts = &runtimeMounts
	}

	// write the runtime config
	runtimeConfig, err := json.MarshalIndent(runtime, "", "    ")
	if err != nil {
		return fmt.Errorf("Failed to create runtime config for %s: %v", image, err)
	}

	hdr = &tar.Header{
		Name: path.Join(prefix, "runtime.json"),
		Mode: 0644,
		Size: int64(len(runtimeConfig)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	buf = bytes.NewBuffer(runtimeConfig)
	if _, err := io.Copy(tw, buf); err != nil {
		return err
	}

	log.Debugf("image bundle: %s %s cfg: %s runtime: %s", prefix, image, string(config), string(runtimeConfig))

	return nil
}
