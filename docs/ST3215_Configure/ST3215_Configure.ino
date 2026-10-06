#include <Arduino.h>

// ============================================================================
// HARDWARE CONFIGURATION
// ============================================================================
#define SERVO_BAUD 115200  // Set to current bus speed (1000000 or 115200)
#define SERVO_RX_PIN 1      // Pico UART0 RX
#define SERVO_TX_PIN 0      // Pico UART0 TX

String inputBuffer = "";

// ST3215 Instruction Set
#define INST_PING       0x01
#define INST_READ       0x02
#define INST_WRITE      0x03

// Official Feetech ST3215 Memory Map Registers
#define REG_ID          0x05 // EEPROM: Servo ID (0-253)
#define REG_BAUD_RATE   0x06 // EEPROM: Baud Rate (0:1M, 1:500k, 2:250k, 3:128k, 4:115.2k) -- FIXED TO 0x06
#define REG_GOAL_POS    0x2A // RAM: Target Position (2 bytes)
#define REG_EEPROM_LOCK 0x37 // RAM: EEPROM Lock (0=Unlocked, 1=Locked)
#define REG_TELEMETRY   0x38 // RAM: Start of Telemetry

struct ServoTelemetry {
  uint16_t currentTick;
  int16_t  currentSpeed;
  int16_t  currentLoad;
  float    voltageV;
  uint8_t  temperatureC;
};

uint8_t calcChecksum(uint8_t *packet, uint8_t length) {
  uint16_t sum = 0;
  for (int i = 2; i < length - 1; i++) {
    sum += packet[i];
  }
  return (uint8_t)(~sum & 0xFF);
}

// ============================================================================
// SERVO COMMUNICATION
// ============================================================================

bool pingServo(uint8_t id) {
  while (Serial1.available() > 0) Serial1.read();

  uint8_t tx[6] = {0xFF, 0xFF, id, 0x02, INST_PING, 0x00};
  tx[5] = calcChecksum(tx, 6);

  Serial1.write(tx, sizeof(tx));
  Serial1.flush();

  unsigned long echoStart = micros();
  int echoBytesToDrain = sizeof(tx);
  while (echoBytesToDrain > 0 && (micros() - echoStart < 5000)) {
    if (Serial1.available() > 0) {
      Serial1.read();
      echoBytesToDrain--;
    }
  }

  uint8_t rx[6];
  int rxIndex = 0;
  unsigned long tStart = millis();

  while ((millis() - tStart < 10) && rxIndex < 6) {
    if (Serial1.available() > 0) {
      rx[rxIndex++] = (uint8_t)Serial1.read();
    }
  }

  if (rxIndex == 6 && rx[0] == 0xFF && rx[1] == 0xFF && rx[2] == id) {
    return (rx[5] == calcChecksum(rx, 6));
  }
  return false;
}

void writeReg8(uint8_t id, uint8_t reg, uint8_t value) {
  uint8_t tx[8] = {0xFF, 0xFF, id, 0x04, INST_WRITE, reg, value, 0x00};
  tx[7] = calcChecksum(tx, 8);
  
  Serial1.write(tx, sizeof(tx));
  Serial1.flush();
}

void writeReg16(uint8_t id, uint8_t reg, uint16_t value) {
  uint8_t tx[9] = {0xFF, 0xFF, id, 0x05, INST_WRITE, reg, 
                   (uint8_t)(value & 0xFF), (uint8_t)((value >> 8) & 0xFF), 0x00};
  tx[8] = calcChecksum(tx, 9);
  
  Serial1.write(tx, sizeof(tx));
  Serial1.flush();
}

bool readTelemetry(uint8_t id, ServoTelemetry &out) {
  while (Serial1.available() > 0) Serial1.read();

  uint8_t tx[8] = {0xFF, 0xFF, id, 0x04, INST_READ, REG_TELEMETRY, 0x08, 0x00};
  tx[7] = calcChecksum(tx, 8);

  Serial1.write(tx, sizeof(tx));
  Serial1.flush();

  unsigned long echoStart = micros();
  int echoBytesToDrain = sizeof(tx);
  while (echoBytesToDrain > 0 && (micros() - echoStart < 5000)) {
    if (Serial1.available() > 0) {
      Serial1.read();
      echoBytesToDrain--;
    }
  }

  uint8_t rx[14];
  int rxIndex = 0;
  unsigned long tStart = millis();

  while ((millis() - tStart < 10) && rxIndex < 14) {
    if (Serial1.available() > 0) {
      rx[rxIndex++] = (uint8_t)Serial1.read();
    }
  }

  if (rxIndex == 14 && rx[0] == 0xFF && rx[1] == 0xFF && rx[2] == id) {
    if (rx[13] == calcChecksum(rx, 14)) {
      out.currentTick   = (uint16_t)(rx[5] | (rx[6] << 8));
      out.currentSpeed  = (int16_t) (rx[7] | (rx[8] << 8));
      out.currentLoad   = (int16_t) (rx[9] | (rx[10] << 8));
      out.voltageV      = (float)rx[11] / 10.0f; 
      out.temperatureC  = rx[12];                
      return true;
    }
  }
  return false;
}

// ============================================================================
// CONFIGURATION ROUTINES
// ============================================================================

void changeServoID(uint8_t oldId, uint8_t newId) {
  Serial.printf("--> Unlocking EEPROM (Reg 0x37) for Servo %d...\n", oldId);
  writeReg8(oldId, REG_EEPROM_LOCK, 0x00);
  delay(20);

  Serial.printf("--> Writing New ID %d to Reg 0x05...\n", newId);
  writeReg8(oldId, REG_ID, newId);
  delay(20);

  Serial.printf("--> Locking EEPROM for Servo %d...\n", newId);
  writeReg8(newId, REG_EEPROM_LOCK, 0x01);
  delay(20);
  
  if (pingServo(newId)) {
    Serial.println("--> SUCCESS: Servo acknowledged new ID!");
  } else {
    Serial.println("--> ERROR: Servo did not acknowledge new ID.");
  }
}

void changeServoBaudrate(uint8_t id, long baud) {
  uint8_t baudCode;
  
  switch (baud) {
    case 1000000: baudCode = 0x00; break; // 1 Mbps
    case 500000:  baudCode = 0x01; break; // 500 kbps
    case 250000:  baudCode = 0x02; break; // 250 kbps
    case 128000:  baudCode = 0x03; break; // 128 kbps
    case 115200:  baudCode = 0x04; break; // 115.2 kbps
    case 76800:   baudCode = 0x05; break; // 76.8 kbps
    case 57600:   baudCode = 0x06; break; // 57.6 kbps
    case 38400:   baudCode = 0x07; break; // 38.4 kbps
    default:
      Serial.println("--> ERROR: Unsupported baud rate.");
      return;
  }

  Serial.printf("--> Unlocking EEPROM (Reg 0x37) for Servo %d...\n", id);
  writeReg8(id, REG_EEPROM_LOCK, 0x00);
  delay(20);

  Serial.printf("--> Writing Baud Code %d to Reg 0x06...\n", baudCode);
  writeReg8(id, REG_BAUD_RATE, baudCode);
  delay(20);

  Serial.printf("--> Locking EEPROM (Reg 0x37) for Servo %d...\n", id);
  writeReg8(id, REG_EEPROM_LOCK, 0x01);
  delay(20);

  Serial.printf("--> SUCCESS: Register 0x06 written to %d (%ld baud).\n", baudCode, baud);
  Serial.println("--> ACTION REQUIRED NOW:");
  Serial.println("    1. Unplug power from the servo, wait 3 seconds, and plug back in.");
  Serial.println("    2. Change '#define SERVO_BAUD 115200' at the top of this code.");
  Serial.println("    3. Reflash the Pico and test with 'PING <id>'.");
}

void recoverServoBus() {
  long bauds[] = {1000000, 115200, 500000, 250000, 128000, 57600, 38400};
  Serial.println("\n--> STARTING BAUD RATE SWEEP...");
  
  for (long b : bauds) {
    Serial.printf("--> Testing UART at %ld baud...\n", b);
    Serial1.begin(b);
    delay(50);
    
    for (int id = 0; id <= 15; id++) {
      if (pingServo(id)) {
        Serial.printf("    [FOUND!] Servo ID %d responded at %ld baud!\n", id, b);
      }
    }
  }
  
  Serial1.begin(SERVO_BAUD);
  Serial.printf("--> Search complete. Reverted Pico UART to %d baud.\n\n", SERVO_BAUD);
}

// ============================================================================
// CLI PARSER
// ============================================================================
void printHelp() {
  Serial.printf("\n=== ST3215 TOOL (CURRENT UART: %d BAUD) ===\n", SERVO_BAUD);
  Serial.println("  SCAN                 - Pings IDs 0-253 on current baud");
  Serial.println("  PING <id>            - Pings specific ID");
  Serial.println("  MOVE <id> <tick>     - Moves servo (0-4095)");
  Serial.println("  READ <id>            - Reads telemetry");
  Serial.println("  SETID <old> <new>    - Changes servo ID");
  Serial.println("  SETBAUD <id> <baud>  - Writes baud rate to Reg 0x06");
  Serial.println("  RECOVER              - Scans all bauds to find lost servos");
  Serial.println("  HELP                 - Prints menu");
  Serial.println("============================================\n");
}

void processCommand(String cmd) {
  cmd.trim();
  cmd.toUpperCase();
  if (cmd.length() == 0) return;

  int space1 = cmd.indexOf(' ');
  int space2 = cmd.indexOf(' ', space1 + 1);

  String action = (space1 == -1) ? cmd : cmd.substring(0, space1);
  String arg1 = (space1 == -1) ? "" : ((space2 == -1) ? cmd.substring(space1 + 1) : cmd.substring(space1 + 1, space2));
  String arg2 = (space2 == -1) ? "" : cmd.substring(space2 + 1);

  if (action == "HELP") {
    printHelp();
  } 
  else if (action == "SCAN") {
    Serial.printf("--> Scanning bus at %d baud...\n", SERVO_BAUD);
    int found = 0;
    for (int i = 0; i <= 253; i++) {
      if (pingServo(i)) {
        Serial.printf("    [+] Found Servo at ID: %d\n", i);
        found++;
      }
    }
    Serial.printf("--> Scan complete. %d servos found.\n", found);
  } 
  else if (action == "RECOVER") {
    recoverServoBus();
  }
  else if (action == "PING") {
    int id = arg1.toInt();
    if (pingServo(id)) {
      Serial.printf("--> Servo %d is ONLINE.\n", id);
    } else {
      Serial.printf("--> Servo %d is OFFLINE.\n", id);
    }
  }
  else if (action == "MOVE") {
    int id = arg1.toInt();
    int pos = arg2.toInt();
    pos = constrain(pos, 0, 4095);
    writeReg16(id, REG_GOAL_POS, pos);
    Serial.printf("--> Sent Servo %d to Tick %d\n", id, pos);
  }
  else if (action == "READ") {
    int id = arg1.toInt();
    ServoTelemetry t;
    if (readTelemetry(id, t)) {
      Serial.printf("--> ID: %3d | Pos: %4d | Spd: %4d | Load: %4d | Volt: %4.1fV | Temp: %2d C\n",
                    id, t.currentTick, t.currentSpeed, t.currentLoad, t.voltageV, t.temperatureC);
    } else {
      Serial.printf("--> Failed to read telemetry from Servo %d\n", id);
    }
  }
  else if (action == "SETID") {
    if (arg1 == "" || arg2 == "") return;
    changeServoID(arg1.toInt(), arg2.toInt());
  }
  else if (action == "SETBAUD") {
    if (arg1 == "" || arg2 == "") {
      Serial.println("--> Usage: SETBAUD <id> <baudrate>");
      return;
    }
    changeServoBaudrate(arg1.toInt(), arg2.toInt());
  }
  else {
    Serial.println("--> Unknown command. Type HELP.");
  }
}

void setup() {
  Serial.begin(115200);
  
  Serial1.setTX(SERVO_TX_PIN);
  Serial1.setRX(SERVO_RX_PIN);
  Serial1.begin(SERVO_BAUD);

  delay(3000); 
  printHelp();
}

void loop() {
  while (Serial.available() > 0) {
    char c = (char)Serial.read();
    if (c == '\n' || c == '\r') {
      if (inputBuffer.length() > 0) {
        processCommand(inputBuffer);
        inputBuffer = "";
      }
    } else {
      inputBuffer += c;
    }
  }
}