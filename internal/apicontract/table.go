package apicontract

import (
	"reflect"
	"strconv"
	"strings"
	"time"
)

const WaitMargin = 5 * time.Second

func Definitions(b Bindings) []Definition { return definitions(reflect.ValueOf(b)) }

func definitions(v reflect.Value) []Definition {
	entries := []Definition{}
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Anonymous {
			entries = append(entries, definitions(v.Field(i))...)
			continue
		}
		tag := v.Type().Field(i).Tag
		access := declaredAccess(tag)
		budget := RequestBytes
		if tag.Get("bytes") != "" {
			budget, _ = strconv.Atoi(tag.Get("bytes"))
		}
		deadline := 30 * time.Second
		if text := tag.Get("seconds"); text != "" {
			seconds, _ := strconv.Atoi(text)
			deadline = time.Duration(seconds) * time.Second
		}
		m := Metadata{Summary: tag.Get("summary"), Name: tag.Get("tool"), Check: tag.Get("check"), Description: tag.Get("desc"), Access: access, Mutation: tag.Get("mutation") == "true", LongPoll: tag.Get("wait") == "true", InputBytes: budget, OutputBytes: ResponseBytes, Deadline: deadline, Get: tag.Get("get"), AdminAction: tag.Get("admin"), Post: tag.Get("post"), Tail: tag.Get("tail") == "200"}
		m.Command, m.CommandFlow = commandMetadata(tag, m.Name)
		entries = append(entries, v.Field(i).Interface().(interface{ Definition(Metadata) Definition }).Definition(m))
	}
	return entries
}

func commandMetadata(tag reflect.StructTag, name string) (string, bool) {
	path := tag.Get("command")
	if path == "" {
		path = strings.ReplaceAll(name, "_", " ")
	}
	return path, tag.Get("flow") == "true"
}
