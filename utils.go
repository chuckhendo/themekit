package phoenix

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
)

func RedText(s string) string {
	return fmt.Sprintf("\033[31m%s\033[0m", s)
}

func YellowText(s string) string {
	return fmt.Sprintf("\033[33m%s\033[0m", s)
}

func BlueText(s string) string {
	return fmt.Sprintf("\033[34m%s\033[0m", s)
}

func TestFixture(name string) string {
	path := fmt.Sprintf("fixtures/%s.json", name)
	file, err := os.Open(path)
	defer file.Close()
	if err != nil {
		log.Fatal(err)
	}
	bytes, err := ioutil.ReadAll(file)
	if err != nil {
		log.Fatal(err)
	}
	return string(bytes)
}