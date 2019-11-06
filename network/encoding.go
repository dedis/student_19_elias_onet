package network

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"sync"

	"go.dedis.ch/kyber/v4/pairing/bn256"
	"go.dedis.ch/kyber/v4/suites"
	"golang.org/x/xerrors"

	"go.dedis.ch/kyber/v4"
	"go.dedis.ch/onet/v4/log"
	"go.dedis.ch/protobuf"
	uuid "gopkg.in/satori/go.uuid.v1"
)

func init() {
	protobuf.RegisterInterface(func() interface{} { return bn256.NewSuiteG1().Point() })
	protobuf.RegisterInterface(func() interface{} { return bn256.NewSuiteG1().Scalar() })
	protobuf.RegisterInterface(func() interface{} { return bn256.NewSuiteG2().Point() })
	protobuf.RegisterInterface(func() interface{} { return bn256.NewSuiteG2().Scalar() })
	protobuf.RegisterInterface(func() interface{} { return bn256.NewSuiteGT().Point() })
	protobuf.RegisterInterface(func() interface{} { return bn256.NewSuiteGT().Scalar() })

	ed25519 := suites.MustFind("Ed25519")
	protobuf.RegisterInterface(func() interface{} { return ed25519.Point() })
	protobuf.RegisterInterface(func() interface{} { return ed25519.Scalar() })
}

/// Encoding part ///

// Suite functionalities used globally by the network library.
type Suite interface {
	kyber.Group
	kyber.Random
}

// Message is a type for any message that the user wants to send
type Message interface{}

// MessageTypeID is the ID used to uniquely identify different registered messages
type MessageTypeID uuid.UUID

// ErrorType is reserved by the network library. When you receive a message of
// ErrorType, it is generally because an error happened, then you can call
// Error() on it.
var ErrorType = MessageTypeID(uuid.Nil)

// String returns the name of the structure if it is known, else it returns
// the hexadecimal value of the Id.
func (mId MessageTypeID) String() string {
	t, ok := registry.get(mId)
	if ok {
		return fmt.Sprintf("PTID(%s:%x)", t.String(), uuid.UUID(mId).Bytes())
	}
	return uuid.UUID(mId).String()
}

// Equal returns true if and only if mID2 equals this MessageTypeID
func (mId MessageTypeID) Equal(mID2 MessageTypeID) bool {
	return uuid.Equal(uuid.UUID(mId), uuid.UUID(mID2))
}

// IsNil returns true iff the MessageTypeID is Nil
func (mId MessageTypeID) IsNil() bool {
	return mId.Equal(MessageTypeID(uuid.Nil))
}

// NamespaceURL is the basic namespace used for uuid
// XXX should move that to external of the library as not every
// cothority/packages should be expected to use that.
const NamespaceURL = "https://dedis.epfl.ch/"

// NamespaceBodyType is the namespace used for PacketTypeID
const NamespaceBodyType = NamespaceURL + "/protocolType/"

var globalOrder = binary.BigEndian

// RegisterMessage registers any struct or ptr and returns the
// corresponding MessageTypeID. Once a struct is registered, it can be sent and
// received by the network library.
func RegisterMessage(msg Message) MessageTypeID {
	msgType := computeMessageType(msg)
	val := reflect.ValueOf(msg)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}
	t := val.Type()
	registry.put(msgType, t)
	return msgType
}

// RegisterMessages is a convenience function to register multiple messages
// together. It returns the MessageTypeIDs of the registered messages. If you
// give the same message more than once, it will register it only once, but return
// it's id as many times as it appears in the arguments.
func RegisterMessages(msg ...Message) []MessageTypeID {
	var ret []MessageTypeID
	for _, m := range msg {
		ret = append(ret, RegisterMessage(m))
	}
	return ret
}

func computeMessageType(msg Message) MessageTypeID {
	val := reflect.ValueOf(msg)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}
	url := NamespaceBodyType + val.Type().String()
	u := uuid.NewV5(uuid.NamespaceURL, url)
	return MessageTypeID(u)
}

// MessageType returns a Message's MessageTypeID if registered or ErrorType if
// the message has not been registered with RegisterMessage().
func MessageType(msg Message) MessageTypeID {
	msgType := computeMessageType(msg)
	_, ok := registry.get(msgType)
	if !ok {
		return ErrorType
	}
	return msgType
}

// Marshal outputs the type and the byte representation of a structure.  It
// first marshals the type as a uuid, i.e. a 16 byte length slice, then the
// struct encoded by protobuf.  That slice of bytes can be then decoded with
// Unmarshal. msg must be a pointer to the message.
func Marshal(msg Message) ([]byte, error) {
	var msgType MessageTypeID
	if msgType = MessageType(msg); msgType == ErrorType {
		return nil, xerrors.Errorf("type of message %s not registered to the network library", reflect.TypeOf(msg))
	}
	b := new(bytes.Buffer)
	if err := binary.Write(b, globalOrder, msgType); err != nil {
		return nil, xerrors.Errorf("buffer write: %v", err)
	}
	var buf []byte
	var err error
	if buf, err = protobuf.Encode(msg); err != nil {
		log.Errorf("Error for protobuf encoding: %s %+v", msg, err)
		if log.DebugVisible() > 0 {
			log.Error(log.Stack())
		}
		return nil, xerrors.Errorf("encoding: %v", err)
	}
	_, err = b.Write(buf)
	if err != nil {
		return nil, xerrors.Errorf("buffer write: %v", err)
	}
	return b.Bytes(), nil
}

// Unmarshal returns the type and the message out of a buffer. One can cast the
// resulting Message to a *pointer* of the underlying type, i.e. it returns a
// pointer.  The type must be registered to the network library in order to be
// decodable and the buffer must have been generated by Marshal otherwise it
// returns an error.
func Unmarshal(buf []byte, suite Suite) (MessageTypeID, Message, error) {
	b := bytes.NewBuffer(buf)
	var tID MessageTypeID
	if err := binary.Read(b, globalOrder, &tID); err != nil {
		return ErrorType, nil, xerrors.Errorf("buffer read: %v", err)
	}
	typ, ok := registry.get(tID)
	if !ok {
		return ErrorType, nil, xerrors.Errorf("type %s not registered", tID.String())
	}
	ptrVal := reflect.New(typ)
	ptr := ptrVal.Interface()
	constructors := DefaultConstructors(suite)
	if err := protobuf.DecodeWithConstructors(b.Bytes(), ptr, constructors); err != nil {
		return ErrorType, nil, xerrors.Errorf("decoding: %v", err)
	}
	return tID, ptrVal.Interface(), nil
}

// DumpTypes is used for debugging - it prints out all known types
func DumpTypes() {
	for t, m := range registry.types {
		log.Print("Type", t, "has message", m)
	}
}

// DefaultConstructors gives a default constructor for protobuf out of the global suite
func DefaultConstructors(suite Suite) protobuf.Constructors {
	constructors := make(protobuf.Constructors)
	if suite != nil {
		var point kyber.Point
		var secret kyber.Scalar
		constructors[reflect.TypeOf(&point).Elem()] = func() interface{} { return suite.Point() }
		constructors[reflect.TypeOf(&secret).Elem()] = func() interface{} { return suite.Scalar() }
	}
	return constructors
}

var registry = newTypeRegistry()

type typeRegistry struct {
	types map[MessageTypeID]reflect.Type
	lock  sync.Mutex
}

func newTypeRegistry() *typeRegistry {
	return &typeRegistry{
		types: make(map[MessageTypeID]reflect.Type),
		lock:  sync.Mutex{},
	}
}

// get returns the reflect.Type corresponding to the registered PacketTypeID
// an a boolean indicating if the type is actually registered or not.
func (tr *typeRegistry) get(mid MessageTypeID) (reflect.Type, bool) {
	tr.lock.Lock()
	defer tr.lock.Unlock()
	t, ok := tr.types[mid]
	return t, ok
}

// put stores the given type in the typeRegistry.
func (tr *typeRegistry) put(mid MessageTypeID, typ reflect.Type) {
	tr.lock.Lock()
	defer tr.lock.Unlock()
	tr.types[mid] = typ
}
