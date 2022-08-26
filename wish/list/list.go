package list

import (
	"sort"
	"strings"

	"git.sr.ht/~erock/pico/wish/send/utils"
	"github.com/charmbracelet/wish"
	"github.com/gliderlabs/ssh"
)

func Middleware(writeHandler utils.CopyFromClientHandler) wish.Middleware {
	return func(sshHandler ssh.Handler) ssh.Handler {
		return func(session ssh.Session) {
			cmd := session.Command()
			if !(len(cmd) > 1 && cmd[0] == "command" && cmd[1] == "ls") {
				sshHandler(session)
				return
			}

			err := writeHandler.Validate(session)
			if err != nil {
				utils.ErrorHandler(session, err)
				return
			}

			fileList, err := writeHandler.List(session, "/")
			if err != nil {
				utils.ErrorHandler(session, err)
				return
			}

			var data []string
			for _, file := range fileList {
				name := strings.ReplaceAll(file.Name(), "/", "")
				if file.IsDir() {
					name += "/"
				}

				data = append(data, name)
			}

			sort.Strings(data)

			_, err = session.Write([]byte(strings.Join(data, "\n")))
			if err != nil {
				utils.ErrorHandler(session, err)
			}
		}
	}
}