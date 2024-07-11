package unknownproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/cosmos/gogoproto/jsonpb"
	"github.com/cosmos/gogoproto/proto"
	"github.com/cosmos/gogoproto/protoc-gen-gogo/descriptor"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/cosmos/cosmos-sdk/codec/types"
)

// RejectUnknownFields rejects any bytes bz with an error that has unknown fields for the provided proto.Message type with an
// option to allow non-critical fields (specified as those fields with bit 11) to pass through. In either case, the
// hasUnknownNonCriticals will be set to true if non-critical fields were encountered during traversal. This flag can be
// used to treat a message with non-critical field different in different security contexts (such as transaction signing).
// This function traverses inside of messages nested via google.protobuf.Any. It does not do any deserialization of the proto.Message.
// An AnyResolver must be provided for traversing inside google.protobuf.Any's.
func RejectUnknownFieldsAndFixTypeUrl(bz []byte, msg proto.Message, allowUnknownNonCriticals bool, resolver jsonpb.AnyResolver) (hasUnknownNonCriticals bool, newBz []byte, err error) {
	if len(bz) == 0 {
		return hasUnknownNonCriticals, nil, nil
	}

	desc, ok := msg.(descriptorIface)
	if !ok {
		return hasUnknownNonCriticals, nil, fmt.Errorf("%T does not have a Descriptor() method", msg)
	}

	fieldDescProtoFromTagNum, _, err := getDescriptorInfo(desc, msg)
	if err != nil {
		return hasUnknownNonCriticals, nil, err
	}

	currentBzOffset := 0
	var updatedBz []byte

	for len(bz) > 0 {
		tagNum, wireType, m := protowire.ConsumeTag(bz)
		if m < 0 {
			return hasUnknownNonCriticals, nil, errors.New("invalid length")
		}

		fieldDescProto, ok := fieldDescProtoFromTagNum[int32(tagNum)]
		switch {
		case ok:
			// Assert that the wireTypes match.
			if !canEncodeType(wireType, fieldDescProto.GetType()) {
				return hasUnknownNonCriticals, nil, &errMismatchedWireType{
					Type:         reflect.ValueOf(msg).Type().String(),
					TagNum:       tagNum,
					GotWireType:  wireType,
					WantWireType: protowire.Type(fieldDescProto.WireType()),
				}
			}

		default:
			isCriticalField := tagNum&bit11NonCritical == 0

			if !isCriticalField {
				hasUnknownNonCriticals = true
			}

			if isCriticalField || !allowUnknownNonCriticals {
				// The tag is critical, so report it.
				return hasUnknownNonCriticals, nil, &errUnknownField{
					Type:     reflect.ValueOf(msg).Type().String(),
					TagNum:   tagNum,
					WireType: wireType,
				}
			}
		}

		/**
		tagNum,wireType: bz[:m]
		fieldBytes: bz[m:n]
		OtherFieldBytes: bz[n:]
		*/
		newBz = append(newBz[:currentBzOffset], bz[:m]...) //append: tagNum,wireType

		// Skip over the bytes that store fieldNumber and wireType bytes.
		bz = bz[m:]
		n := protowire.ConsumeFieldValue(tagNum, wireType, bz)
		if n < 0 {
			err = fmt.Errorf("could not consume field value for tagNum: %d, wireType: %q; %w",
				tagNum, wireTypeToString(wireType), protowire.ParseError(n))
			return hasUnknownNonCriticals, nil, err
		}

		newBz = append(newBz, bz[:n]...) //append: original fieldBytes

		fieldBytes := bz[:n]
		bz = bz[n:]

		// An unknown but non-critical field or just a scalar type (aka *INT and BYTES like).
		if fieldDescProto == nil || fieldDescProto.IsScalar() {
			currentBzOffset = currentBzOffset + m + n
			continue
		}

		protoMessageName := fieldDescProto.GetTypeName()
		if protoMessageName == "" {
			switch typ := fieldDescProto.GetType(); typ {
			case descriptor.FieldDescriptorProto_TYPE_STRING, descriptor.FieldDescriptorProto_TYPE_BYTES:

				// At this point only TYPE_STRING is expected to be unregistered, since FieldDescriptorProto.IsScalar() returns false for
				// TYPE_BYTES and TYPE_STRING as per
				// https://github.com/cosmos/gogoproto/blob/5628607bb4c51c3157aacc3a50f0ab707582b805/protoc-gen-gogo/descriptor/descriptor.go#L95-L118
			default:
				return hasUnknownNonCriticals, nil, fmt.Errorf("failed to get typename for message of type %v, can only be TYPE_STRING or TYPE_BYTES", typ)
			}
			currentBzOffset = currentBzOffset + m + n
			continue
		}

		// Let's recursively traverse and typecheck the field.

		// consume length prefix of nested message
		_, o := protowire.ConsumeVarint(fieldBytes)
		fieldBytesLengthPrefix := fieldBytes[:o]
		fieldBytes = fieldBytes[o:]

		var msg proto.Message
		var err error

		if protoMessageName == ".google.protobuf.Any" {
			// Firstly typecheck types.Any to ensure nothing snuck in.
			hasUnknownNonCriticalsChild, _, err := RejectUnknownFieldsAndFixTypeUrl(fieldBytes, (*types.Any)(nil), allowUnknownNonCriticals, resolver)
			hasUnknownNonCriticals = hasUnknownNonCriticals || hasUnknownNonCriticalsChild
			if err != nil {
				return hasUnknownNonCriticals, nil, err
			}
			// And finally we can extract the TypeURL containing the protoMessageName.
			any := new(types.Any)
			if err := proto.Unmarshal(fieldBytes, any); err != nil {
				return hasUnknownNonCriticals, nil, err
			}
			protoMessageName = any.TypeUrl
			//update type-url
			if any.TypeUrl == "/stratos.register.v1.MsgCreateResourceNode" ||
				any.TypeUrl == "/stratos.register.v1.MsgUpdateResourceNode" ||
				any.TypeUrl == "/stratos.register.v1.MsgCreateMetaNode" ||
				any.TypeUrl == "/stratos.register.v1.MsgUpdateMetaNode" {
				any.TypeUrl = strings.Replace(protoMessageName, "v1", "v1_1", 1)
				protoMessageName = any.TypeUrl

				//re-marshal to bytes with changed typeUrl
				newFieldBytesWithTypeUrl, err := proto.Marshal(any)
				if err != nil {
					return hasUnknownNonCriticals, nil, err
				}
				// repack with length prefix
				var sizeBuf [binary.MaxVarintLen64]byte
				num := binary.PutUvarint(sizeBuf[:], uint64(any.Size()))
				repackedBzWithLengthPrefix := append(sizeBuf[:num], newFieldBytesWithTypeUrl...)
				n = len(repackedBzWithLengthPrefix)

				// consume length prefix of nested message
				_, o = protowire.ConsumeVarint(repackedBzWithLengthPrefix)
				fieldBytesLengthPrefix = repackedBzWithLengthPrefix[:o]
				fieldBytes = repackedBzWithLengthPrefix[o:]
			}

			updatedBz = append(fieldBytesLengthPrefix, fieldBytes...)
			updatedBz = append(fieldBytesLengthPrefix, fieldBytes...)
			newBz = append(newBz[:currentBzOffset+m], updatedBz...) // replace fieldBytes & following bytes

			fieldBytes = any.Value
			msg, err = resolver.Resolve(protoMessageName)
			if err != nil {
				return hasUnknownNonCriticals, nil, err
			}
		} else {
			msg, err = protoMessageForTypeName(protoMessageName[1:])
			if err != nil {
				return hasUnknownNonCriticals, nil, err
			}
		}

		hasUnknownNonCriticalsChild, _, err := RejectUnknownFieldsAndFixTypeUrl(fieldBytes, msg, allowUnknownNonCriticals, resolver)
		hasUnknownNonCriticals = hasUnknownNonCriticals || hasUnknownNonCriticalsChild
		if err != nil {
			return hasUnknownNonCriticals, nil, err
		}

		currentBzOffset = currentBzOffset + m + n
	}

	return hasUnknownNonCriticals, newBz, nil
}
