# Packet — examples

## 1. TCP header

What this shows: the full TCP header field layout using explicit bit ranges, including single-bit flag fields.

```mermaid
---
title: "TCP Packet"
---
packet
0-15: "Source Port"
16-31: "Destination Port"
32-63: "Sequence Number"
64-95: "Acknowledgment Number"
96-99: "Data Offset"
100-105: "Reserved"
106: "URG"
107: "ACK"
108: "PSH"
109: "RST"
110: "SYN"
111: "FIN"
112-127: "Window"
128-143: "Checksum"
144-159: "Urgent Pointer"
160-191: "(Options and Padding)"
192-255: "Data (variable length)"
```

## 2. UDP header (bit-count shorthand)

What this shows: the same kind of header expressed with `+count` instead of manual bit math for the fixed-width fields.

```mermaid
packet
title UDP Packet
+16: "Source Port"
+16: "Destination Port"
32-47: "Length"
48-63: "Checksum"
64-95: "Data (variable length)"
```

## 3. IPv4 header

What this shows: a denser header with several sub-byte fields (version/IHL share a byte, flags share bits with fragment offset).

```mermaid
packet
title IPv4 Header
0-3: "Version"
4-7: "IHL"
8-15: "Type of Service"
16-31: "Total Length"
32-47: "Identification"
48-50: "Flags"
51-63: "Fragment Offset"
64-71: "TTL"
72-79: "Protocol"
80-95: "Header Checksum"
96-127: "Source Address"
128-159: "Destination Address"
```

## 4. Custom application packet

What this shows: a hand-rolled binary protocol (game netcode) documented the same way as a standard header.

```mermaid
packet
title Game Netcode Packet
+8: "Message Type"
+8: "Protocol Version"
+16: "Sequence Number"
+32: "Session ID"
+16: "Payload Length"
+32: "Checksum"
```
