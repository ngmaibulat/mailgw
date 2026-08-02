import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class linkAddrTransaction extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

linkAddrTransaction.init(
    {
        MailAddrId: DataTypes.INTEGER,
        TransactionId: DataTypes.INTEGER,
    },
    {
        sequelize,
        modelName: "linkAddrTransaction",
    }
);

export default linkAddrTransaction;
